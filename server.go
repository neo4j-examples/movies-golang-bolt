package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

type MovieResult struct {
	Movie `json:"movie"`
}

type VoteResult struct {
	Updates int `json:"updates"`
}

type Movie struct {
	Released int64    `json:"released"`
	Title    string   `json:"title,omitempty"`
	Tagline  string   `json:"tagline,omitempty"`
	Votes    int64    `json:"votes,omitempty"`
	Cast     []Person `json:"cast,omitempty"`
}

type Person struct {
	Job  string   `json:"job"`
	Role []string `json:"role"`
	Name string   `json:"name"`
}

type D3Response struct {
	Nodes []Node `json:"nodes"`
	Links []Link `json:"links"`
}

type Node struct {
	Title string `json:"title"`
	Label string `json:"label"`
}

type Link struct {
	Source int `json:"source"`
	Target int `json:"target"`
}

type Neo4jConfiguration struct {
	Url      string
	Username string
	Password string
	Database string
}

func (nc *Neo4jConfiguration) newDriver() (neo4j.DriverWithContext, error) {
	return neo4j.NewDriverWithContext(nc.Url, neo4j.BasicAuth(nc.Username, nc.Password, ""))
}

func defaultHandler(w http.ResponseWriter, req *http.Request) {
	_, file, _, _ := runtime.Caller(0)
	page := filepath.Join(filepath.Dir(file), "public", "index.html")
	fmt.Printf("Serving HTML file %s\n", page)
	if body, err := os.ReadFile(page); err != nil {
		w.WriteHeader(500)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(err.Error()))
	} else {
		w.Header().Set("Content-Type", "text/html;charset=utf-8")
		_, _ = w.Write(body)
	}
}

func searchHandlerFunc(ctx context.Context, driver neo4j.DriverWithContext, database string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		result, err := neo4j.ExecuteQuery(ctx, driver, `MATCH (movie:Movie)
				 WHERE toLower(movie.title) CONTAINS toLower($title)
				 RETURN movie.title AS title, movie.tagline AS tagline, movie.votes AS votes, movie.released AS released`,
			map[string]interface{}{"title": req.URL.Query().Get("q")},
			neo4j.EagerResultTransformer,
			neo4j.ExecuteQueryWithReadersRouting(),
			neo4j.ExecuteQueryWithDatabase(database))
		if err != nil {
			log.Println("error querying search:", err)
			return
		}

		movies := make([]MovieResult, len(result.Records))
		for i, record := range result.Records {
			released, _, _ := neo4j.GetRecordValue[int64](record, "released")
			title, _, _ := neo4j.GetRecordValue[string](record, "title")
			tagline, _, _ := neo4j.GetRecordValue[string](record, "tagline")
			votes, _, _ := neo4j.GetRecordValue[int64](record, "votes")
			movies[i] = MovieResult{Movie{Released: released, Title: title, Tagline: tagline, Votes: votes}}
		}
		err = json.NewEncoder(w).Encode(movies)
		if err != nil {
			log.Println("error writing search response:", err)
		}
	}
}

func movieHandlerFunc(ctx context.Context, driver neo4j.DriverWithContext, database string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		title, _ := url.QueryUnescape(req.URL.Path[len("/movie/"):])
		result, err := neo4j.ExecuteQuery(ctx, driver, `MATCH (movie:Movie {title:$title})
				OPTIONAL MATCH (movie)<-[r]-(person:Person)
				WITH movie.title AS title,
					 collect({
						name:person.name,
						job: head(split(toLower(type(r)),'_')),
						role: r.roles
					}) AS cast
				LIMIT 1
				UNWIND cast as c
				RETURN title, c.name as name, c.job as job, c.role as role`,
			map[string]interface{}{"title": title},
			neo4j.EagerResultTransformer,
			neo4j.ExecuteQueryWithReadersRouting(),
			neo4j.ExecuteQueryWithDatabase(database))

		var movie Movie
		for _, record := range result.Records {
			title, _, _ := neo4j.GetRecordValue[string](record, "title")
			movie.Title = title
			name, _, _ := neo4j.GetRecordValue[string](record, "name")
			job, _, _ := neo4j.GetRecordValue[string](record, "job")
			role, _ := record.Get("role")
			switch role.(type) {
			case []any:
				movie.Cast = append(movie.Cast, Person{Name: name, Job: job, Role: toStringSlice(role.([]any))})
			default: // handle nulls or unexpected stuff
				movie.Cast = append(movie.Cast, Person{Name: name, Job: job})
			}
		}
		if err != nil {
			log.Println("error querying movie:", err)
			return
		}
		err = json.NewEncoder(w).Encode(movie)
		if err != nil {
			log.Println("error writing movie response:", err)
		}
	}
}

func voteInMovieHandlerFunc(ctx context.Context, driver neo4j.DriverWithContext, database string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		title, _ := url.QueryUnescape(req.URL.Path[len("/movie/vote/"):])
		result, err := neo4j.ExecuteQuery(ctx, driver, `MATCH (m:Movie {title: $title})
				SET m.votes = coalesce(m.votes, 0) + 1`,
			map[string]interface{}{"title": title},
			neo4j.EagerResultTransformer,
			neo4j.ExecuteQueryWithDatabase(database))

		var vote VoteResult
		vote.Updates = result.Summary.Counters().PropertiesSet()

		if err != nil {
			log.Println("error voting for movie:", err)
			return
		}
		err = json.NewEncoder(w).Encode(vote)
		if err != nil {
			log.Println("error writing vote result response:", err)
		}
	}
}

func graphHandler(ctx context.Context, driver neo4j.DriverWithContext, database string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		result, err := neo4j.ExecuteQuery(ctx, driver, `MATCH (m:Movie)<-[:ACTED_IN]-(a:Person)
				  RETURN m.title AS movie, collect(a.name) AS cast
				  LIMIT $limit `,
			map[string]interface{}{"limit": parseLimit(req)},
			neo4j.EagerResultTransformer,
			neo4j.ExecuteQueryWithReadersRouting(),
			neo4j.ExecuteQueryWithDatabase(database))

		var d3Response D3Response
		for _, record := range result.Records {
			title, _, _ := neo4j.GetRecordValue[string](record, "movie")
			actors, _, _ := neo4j.GetRecordValue[[]any](record, "cast")
			d3Response.Nodes = append(d3Response.Nodes, Node{Title: title, Label: "movie"})
			movIdx := len(d3Response.Nodes) - 1
			for _, actor := range actors {
				idx := -1
				for i, node := range d3Response.Nodes {
					if actor == node.Title && node.Label == "actor" {
						idx = i
						break
					}
				}
				if idx == -1 {
					d3Response.Nodes = append(d3Response.Nodes, Node{Title: actor.(string), Label: "actor"})
					d3Response.Links = append(d3Response.Links, Link{Source: len(d3Response.Nodes) - 1, Target: movIdx})
				} else {
					d3Response.Links = append(d3Response.Links, Link{Source: idx, Target: movIdx})
				}
			}
		}
		if err != nil {
			log.Println("error querying graph:", err)
			return
		}
		err = json.NewEncoder(w).Encode(d3Response)
		if err != nil {
			log.Println("error writing graph response:", err)
		}
	}
}

func toStringSlice(slice []interface{}) []string {
	var result []string
	for _, e := range slice {
		result = append(result, e.(string))
	}
	return result
}

func main() {
	ctx := context.Background()
	configuration := parseConfiguration()
	driver, err := configuration.newDriver()
	if err != nil {
		log.Fatal(err)
	}
	defer unsafeClose(ctx, driver)
	serveMux := http.NewServeMux()
	serveMux.HandleFunc("/", defaultHandler)
	serveMux.HandleFunc("/search", searchHandlerFunc(ctx, driver, configuration.Database))
	serveMux.HandleFunc("/movie/vote/", voteInMovieHandlerFunc(ctx, driver, configuration.Database))
	serveMux.HandleFunc("/movie/", movieHandlerFunc(ctx, driver, configuration.Database))
	serveMux.HandleFunc("/graph", graphHandler(ctx, driver, configuration.Database))

	var port string
	var found bool
	if port, found = os.LookupEnv("PORT"); !found {
		port = "8080"
	}
	fmt.Printf("Running on port %s, database is at %s\n", port, configuration.Url)
	panic(http.ListenAndServe(":"+port, serveMux))
}

func parseLimit(req *http.Request) int {
	limits := req.URL.Query()["limit"]
	limit := 50
	if len(limits) > 0 {
		var err error
		if limit, err = strconv.Atoi(limits[0]); err != nil {
			limit = 50
		}
	}
	return limit
}

func parseConfiguration() *Neo4jConfiguration {
	database := lookupEnvOrGetDefault("NEO4J_DATABASE", "movies")
	if !strings.HasPrefix(lookupEnvOrGetDefault("NEO4J_VERSION", "4"), "4") {
		database = ""
	}
	return &Neo4jConfiguration{
		Url:      lookupEnvOrGetDefault("NEO4J_URI", "neo4j+s://demo.neo4jlabs.com"),
		Username: lookupEnvOrGetDefault("NEO4J_USER", "movies"),
		Password: lookupEnvOrGetDefault("NEO4J_PASSWORD", "movies"),
		Database: database,
	}
}

func lookupEnvOrGetDefault(key string, defaultValue string) string {
	if env, found := os.LookupEnv(key); !found {
		return defaultValue
	} else {
		return env
	}
}

func unsafeClose(ctx context.Context, closeable interface{ Close(context.Context) error }) {
	if err := closeable.Close(ctx); err != nil {
		log.Fatal(fmt.Errorf("could not close resource: %w", err))
	}
}
