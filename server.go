package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

//go:embed public/index.html
var indexPage []byte

type MovieResult struct {
	Movie `json:"movie"`
}

type VoteResult struct {
	Updates int `json:"updates"`
}

type Movie struct {
	Released int64    `json:"released" neo4j:"released"`
	Title    string   `json:"title,omitempty" neo4j:"title"`
	Tagline  string   `json:"tagline,omitempty" neo4j:"tagline"`
	Votes    int64    `json:"votes,omitempty" neo4j:"votes"`
	Cast     []Person `json:"cast,omitempty" neo4j:"-"`
}

type Person struct {
	Job  string   `json:"job" neo4j:"job"`
	Role []string `json:"role" neo4j:"role"`
	Name string   `json:"name" neo4j:"name"`
}

type MovieCast struct {
	Title  string   `neo4j:"movie"`
	Actors []string `neo4j:"cast"`
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

func (nc *Neo4jConfiguration) newDriver() (neo4j.Driver, error) {
	return neo4j.NewDriver(nc.Url, neo4j.BasicAuth(nc.Username, nc.Password, ""))
}

func defaultHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html;charset=utf-8")
	_, _ = w.Write(indexPage)
}

func searchHandlerFunc(driver neo4j.Driver, database string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		result, err := neo4j.ExecuteQuery(req.Context(), driver, `MATCH (movie:Movie)
				 WHERE toLower(movie.title) CONTAINS toLower($title)
				 RETURN movie.title AS title, movie.tagline AS tagline, movie.votes AS votes, movie.released AS released`,
			map[string]any{"title": req.URL.Query().Get("q")},
			neo4j.EagerResultTransformer,
			neo4j.ExecuteQueryWithReadersRouting(),
			neo4j.ExecuteQueryWithDatabase(database))
		if err != nil {
			serverError(w, "error querying search:", err)
			return
		}

		movies, err := neo4j.CollectRecordsAs[MovieResult](result.Records)
		if err != nil {
			serverError(w, "error mapping search results:", err)
			return
		}
		err = json.NewEncoder(w).Encode(movies)
		if err != nil {
			log.Println("error writing search response:", err)
		}
	}
}

func movieHandlerFunc(driver neo4j.Driver, database string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		result, err := neo4j.ExecuteQuery(req.Context(), driver, `MATCH (movie:Movie {title:$title})
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
			map[string]any{"title": req.PathValue("title")},
			neo4j.EagerResultTransformer,
			neo4j.ExecuteQueryWithReadersRouting(),
			neo4j.ExecuteQueryWithDatabase(database))
		if err != nil {
			serverError(w, "error querying movie:", err)
			return
		}

		cast, err := neo4j.CollectRecordsAs[Person](result.Records)
		if err != nil {
			serverError(w, "error mapping movie cast:", err)
			return
		}
		movie := Movie{Cast: cast}
		if len(result.Records) > 0 {
			movie.Title, _, _ = neo4j.GetRecordValue[string](result.Records[0], "title")
		}
		err = json.NewEncoder(w).Encode(movie)
		if err != nil {
			log.Println("error writing movie response:", err)
		}
	}
}

func voteInMovieHandlerFunc(driver neo4j.Driver, database string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		result, err := neo4j.ExecuteQuery(req.Context(), driver, `MATCH (m:Movie {title: $title})
				SET m.votes = coalesce(m.votes, 0) + 1`,
			map[string]any{"title": req.PathValue("title")},
			neo4j.EagerResultTransformer,
			neo4j.ExecuteQueryWithDatabase(database))
		if err != nil {
			serverError(w, "error voting for movie:", err)
			return
		}

		vote := VoteResult{Updates: result.Summary.Counters().PropertiesSet()}
		err = json.NewEncoder(w).Encode(vote)
		if err != nil {
			log.Println("error writing vote result response:", err)
		}
	}
}

func graphHandler(driver neo4j.Driver, database string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		result, err := neo4j.ExecuteQuery(req.Context(), driver, `MATCH (m:Movie)<-[:ACTED_IN]-(a:Person)
				  RETURN m.title AS movie, collect(a.name) AS cast
				  LIMIT $limit `,
			map[string]any{"limit": parseLimit(req)},
			neo4j.EagerResultTransformer,
			neo4j.ExecuteQueryWithReadersRouting(),
			neo4j.ExecuteQueryWithDatabase(database))
		if err != nil {
			serverError(w, "error querying graph:", err)
			return
		}

		movies, err := neo4j.CollectRecordsAs[MovieCast](result.Records)
		if err != nil {
			serverError(w, "error mapping graph:", err)
			return
		}
		var d3Response D3Response
		allActors := make(map[string]int)
		for _, movie := range movies {
			d3Response.Nodes = append(d3Response.Nodes, Node{Title: movie.Title, Label: "movie"})
			movieIndex := len(d3Response.Nodes) - 1
			for _, actorName := range movie.Actors {
				if actorIndex, found := allActors[actorName]; found {
					d3Response.Links = append(d3Response.Links, Link{Source: actorIndex, Target: movieIndex})
				} else {
					d3Response.Nodes = append(d3Response.Nodes, Node{Title: actorName, Label: "actor"})
					newActorIndex := len(d3Response.Nodes) - 1
					d3Response.Links = append(d3Response.Links, Link{Source: newActorIndex, Target: movieIndex})
					allActors[actorName] = newActorIndex
				}
			}
		}
		err = json.NewEncoder(w).Encode(d3Response)
		if err != nil {
			log.Println("error writing graph response:", err)
		}
	}
}

func serverError(w http.ResponseWriter, message string, err error) {
	log.Println(message, err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx := context.Background()
	configuration := parseConfiguration()
	driver, err := configuration.newDriver()
	if err != nil {
		return err
	}
	defer func() {
		if err := driver.Close(ctx); err != nil {
			log.Println("error closing driver:", err)
		}
	}()
	if err := driver.VerifyConnectivity(ctx); err != nil {
		return fmt.Errorf("could not connect to %s: %w", configuration.Url, err)
	}

	serveMux := http.NewServeMux()
	serveMux.HandleFunc("GET /", defaultHandler)
	serveMux.HandleFunc("GET /search", searchHandlerFunc(driver, configuration.Database))
	serveMux.HandleFunc("GET /movie/{title}", movieHandlerFunc(driver, configuration.Database))
	serveMux.HandleFunc("POST /movie/vote/{title}", voteInMovieHandlerFunc(driver, configuration.Database))
	serveMux.HandleFunc("GET /graph", graphHandler(driver, configuration.Database))

	port := lookupEnvOrGetDefault("PORT", "8080")
	fmt.Printf("Running on port %s, database is at %s\n", port, configuration.Url)
	return http.ListenAndServe(":"+port, serveMux)
}

func parseLimit(req *http.Request) int {
	limit, err := strconv.Atoi(req.URL.Query().Get("limit"))
	if err != nil {
		return 50
	}
	return limit
}

func parseConfiguration() *Neo4jConfiguration {
	return &Neo4jConfiguration{
		Url:      lookupEnvOrGetDefault("NEO4J_URI", "neo4j+s://demo.neo4jlabs.com"),
		Username: lookupEnvOrGetDefault("NEO4J_USER", "movies"),
		Password: lookupEnvOrGetDefault("NEO4J_PASSWORD", "movies"),
		Database: lookupEnvOrGetDefault("NEO4J_DATABASE", "movies"),
	}
}

func lookupEnvOrGetDefault(key string, defaultValue string) string {
	if env, found := os.LookupEnv(key); found {
		return env
	}
	return defaultValue
}
