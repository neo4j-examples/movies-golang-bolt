package main

import (
	"encoding/json"
	"fmt"
	"github.com/neo4j/neo4j-go-driver/neo4j"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/daaku/go.httpgzip"
)

type MovieResult struct {
	Movie `json:"movie"`
}

type Movie struct {
	Released int64    `json:"released"`
	Title    string   `json:"title,omitempty"`
	Tagline  string   `json:"tagline,omitempty"`
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

func (nc *Neo4jConfiguration) newDriver() (neo4j.Driver, error) {
	unencrypted := func(conf *neo4j.Config) { conf.Encrypted = false }
	return neo4j.NewDriver(nc.Url, neo4j.BasicAuth(nc.Username, nc.Password, ""), unencrypted)
}

func defaultHandler(w http.ResponseWriter, req *http.Request) {
	if body, err := ioutil.ReadFile("public/index.html"); err != nil {
		w.WriteHeader(500)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(err.Error()))
	} else {
		w.Header().Set("Content-Type", "text/html;charset=utf-8")
		w.Write(body)
	}
}

func searchHandlerFunc(driver neo4j.Driver, database string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		session, err := driver.NewSession(neo4j.SessionConfig{
			AccessMode:   neo4j.AccessModeRead,
			DatabaseName: database,
		})
		if err != nil {
			internalServerError(w, err)
			return
		}
		defer unsafeClose(session)

		query := `MATCH (movie:Movie) 
				 WHERE movie.title =~ $title
				 RETURN movie.title as title, movie.tagline as tagline, movie.released as released`

		titleRegex := fmt.Sprintf("(?i).*%s.*", req.URL.Query()["q"][0])
		result, err := session.Run(query, map[string]interface{}{"title": titleRegex})
		if err != nil {
			log.Println("error querying search:", err)
		}
		var movieResults []MovieResult
		for result.Next() {
			record := result.Record()
			released, _ := record.Get("released")
			title, _ := record.Get("title")
			tagline, _ := record.Get("tagline")
			movieResults = append(movieResults, MovieResult{Movie{
				Released: released.(int64),
				Title:    title.(string),
				Tagline:  tagline.(string),
			}})
		}

		err = json.NewEncoder(w).Encode(movieResults)
		if err != nil {
			log.Println("error writing search response:", err)
		}
	}
}

func movieHandlerFunc(driver neo4j.Driver, database string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		session, err := driver.NewSession(neo4j.SessionConfig{
			AccessMode:   neo4j.AccessModeRead,
			DatabaseName: database,
		})
		if err != nil {
			internalServerError(w, err)
			return
		}
		defer unsafeClose(session)

		query := `MATCH (movie:Movie {title:$title})
				  OPTIONAL MATCH (movie)<-[r]-(person:Person)
				  WITH movie.title as title,
						 collect({name:person.name,
						 job:head(split(toLower(type(r)),'_')),
						 role:r.roles}) as cast 
				  LIMIT 1
				  UNWIND cast as c 
				  RETURN title, c.name as name, c.job as job, c.role as role`

		title := req.URL.Path[len("/movie/"):]
		result, err := session.Run(query, map[string]interface{}{"title": title})
		if err != nil {
			log.Println("error querying movie:", err)
		}
		var movie Movie
		for result.Next() {
			record := result.Record()
			title, _ := record.Get("title")
			movie.Title = title.(string)
			name, _ := record.Get("name")
			job, _ := record.Get("job")
			role, _ := record.Get("role")
			switch role.(type) {
			case []interface{}:
				movie.Cast = append(movie.Cast, Person{Name: name.(string), Job: job.(string), Role: toStringSlice(role.([]interface{}))})
			default: // handle nulls or unexpected stuff
				movie.Cast = append(movie.Cast, Person{Name: name.(string), Job: job.(string)})
			}
		}

		err = json.NewEncoder(w).Encode(movie)
		if err != nil {
			log.Println("error writing movie response:", err)
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

func graphHandler(driver neo4j.Driver, database string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		session, err := driver.NewSession(neo4j.SessionConfig{
			AccessMode:   neo4j.AccessModeRead,
			DatabaseName: database,
		})
		if err != nil {
			internalServerError(w, err)
			return
		}
		defer unsafeClose(session)

		limit := parseLimit(req)
		query := `MATCH (m:Movie)<-[:ACTED_IN]-(a:Person)
				  RETURN m.title as movie, collect(a.name) as cast
				  LIMIT $limit `

		result, err := session.Run(query, map[string]interface{}{"limit": limit})
		if err != nil {
			log.Println("error querying graph:", err)
		}

		d3Resp := D3Response{}
		for result.Next() {
			record := result.Record()
			title, _ := record.Get("movie")
			actors, _ := record.Get("cast")
			d3Resp.Nodes = append(d3Resp.Nodes, Node{Title: title.(string), Label: "movie"})
			movIdx := len(d3Resp.Nodes) - 1
			for _, actor := range actors.([]interface{}) {
				idx := -1
				for i, node := range d3Resp.Nodes {
					if actor == node.Title && node.Label == "actor" {
						idx = i
						break
					}
				}
				if idx == -1 {
					d3Resp.Nodes = append(d3Resp.Nodes, Node{Title: actor.(string), Label: "actor"})
					d3Resp.Links = append(d3Resp.Links, Link{Source: len(d3Resp.Nodes) - 1, Target: movIdx})
				} else {
					d3Resp.Links = append(d3Resp.Links, Link{Source: idx, Target: movIdx})
				}
			}
		}

		err = json.NewEncoder(w).Encode(d3Resp)
		if err != nil {
			log.Println("error writing graph response:", err)
		}
	}
}

func main() {
	configuration := parseConfiguration()
	driver, err := configuration.newDriver()
	if err != nil {
		log.Fatal(err)
	}
	defer unsafeClose(driver)
	serveMux := http.NewServeMux()
	serveMux.HandleFunc("/", defaultHandler)
	serveMux.HandleFunc("/search", searchHandlerFunc(driver, configuration.Database))
	serveMux.HandleFunc("/movie/", movieHandlerFunc(driver, configuration.Database))
	serveMux.HandleFunc("/graph", graphHandler(driver, configuration.Database))

	var port string
	var found bool
	if port, found = os.LookupEnv("PORT"); !found {
		port = "8080"
	}
	panic(http.ListenAndServe(":"+port, httpgzip.NewHandler(serveMux)))
}

func internalServerError(w http.ResponseWriter, err error) {
	w.WriteHeader(500)
	w.Header().Set("Content-Type", "text/plain")
	w.Write([]byte(fmt.Errorf("unexpected error: %w", err).Error()))
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
	return &Neo4jConfiguration{
		Url:      fmt.Sprintf("bolt://%s", lookupEnvOrGetDefault("NEO4J_HOST", "demo.neo4jlabs.com/movies")),
		Username: lookupEnvOrGetDefault("NEO4J_USER", "movies"),
		Password: lookupEnvOrGetDefault("NEO4J_PASS", "movies"),
		Database: lookupEnvOrGetDefault("NEO4J_DATABASE", "movies"),
	}
}

func lookupEnvOrGetDefault(key string, defaultValue string) string {
	if env, found := os.LookupEnv(key); !found {
		return defaultValue
	} else {
		return env
	}
}

func unsafeClose(closeable io.Closer) {
	if err := closeable.Close(); err != nil {
		log.Fatal(fmt.Errorf("could not close resource: %w", err))
	}
}
