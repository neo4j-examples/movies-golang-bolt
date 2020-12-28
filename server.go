package main

import (
	"encoding/json"
	"fmt"
	"github.com/neo4j/neo4j-go-driver/v4/neo4j"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

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
	return neo4j.NewDriver(nc.Url, neo4j.BasicAuth(nc.Username, nc.Password, ""))
}

func defaultHandler(w http.ResponseWriter, req *http.Request) {
	if body, err := ioutil.ReadFile("public/index.html"); err != nil {
		w.WriteHeader(500)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(err.Error()))
	} else {
		w.Header().Set("Content-Type", "text/html;charset=utf-8")
		_, _ = w.Write(body)
	}
}

func searchHandlerFunc(driver neo4j.Driver, database string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		session := driver.NewSession(neo4j.SessionConfig{
			AccessMode:   neo4j.AccessModeRead,
			DatabaseName: database,
		})
		defer unsafeClose(session)

		movieResults, err := session.ReadTransaction(func(tx neo4j.Transaction) (interface{}, error) {
			records, err := tx.Run(
				`MATCH (movie:Movie) 
				 WHERE movie.title =~ $title
				 RETURN movie.title as title, movie.tagline as tagline, movie.released as released`,
				map[string]interface{}{"title": fmt.Sprintf("(?i).*%s.*", req.URL.Query()["q"][0])})
			if err != nil {
				return nil, err
			}
			var results []MovieResult
			for records.Next() {
				record := records.Record()
				released, _ := record.Get("released")
				title, _ := record.Get("title")
				tagline, _ := record.Get("tagline")
				results = append(results, MovieResult{Movie{
					Released: released.(int64),
					Title:    title.(string),
					Tagline:  tagline.(string),
				}})
			}
			return results, nil
		})
		if err != nil {
			log.Println("error querying search:", err)
			return
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

		session := driver.NewSession(neo4j.SessionConfig{
			AccessMode:   neo4j.AccessModeRead,
			DatabaseName: database,
		})
		defer unsafeClose(session)

		movie, err := session.ReadTransaction(func(tx neo4j.Transaction) (interface{}, error) {
			title, _ := url.QueryUnescape(req.URL.Path[len("/movie/"):])
			records, err := tx.Run(
				`MATCH (movie:Movie {title:$title})
				  OPTIONAL MATCH (movie)<-[r]-(person:Person)
				  WITH movie.title as title,
						 collect({name:person.name,
						 job:head(split(toLower(type(r)),'_')),
						 role:r.roles}) as cast 
				  LIMIT 1
				  UNWIND cast as c 
				  RETURN title, c.name as name, c.job as job, c.role as role`,
				map[string]interface{}{"title": title})
			if err != nil {
				return nil, err
			}
			var result Movie
			for records.Next() {
				record := records.Record()
				title, _ := record.Get("title")
				result.Title = title.(string)
				name, _ := record.Get("name")
				job, _ := record.Get("job")
				role, _ := record.Get("role")
				switch role.(type) {
				case []interface{}:
					result.Cast = append(result.Cast, Person{Name: name.(string), Job: job.(string), Role: toStringSlice(role.([]interface{}))})
				default: // handle nulls or unexpected stuff
					result.Cast = append(result.Cast, Person{Name: name.(string), Job: job.(string)})
				}
			}
			return result, nil
		})
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

		session := driver.NewSession(neo4j.SessionConfig{
			AccessMode:   neo4j.AccessModeRead,
			DatabaseName: database,
		})
		defer unsafeClose(session)

		limit := parseLimit(req)
		query := `MATCH (m:Movie)<-[:ACTED_IN]-(a:Person)
				  RETURN m.title as movie, collect(a.name) as cast
				  LIMIT $limit `

		d3Resp, err := session.ReadTransaction(func(tx neo4j.Transaction) (interface{}, error) {
			records, err := tx.Run(query, map[string]interface{}{"limit": limit})
			if err != nil {
				return nil, err
			}
			result := D3Response{}
			for records.Next() {
				record := records.Record()
				title, _ := record.Get("movie")
				actors, _ := record.Get("cast")
				result.Nodes = append(result.Nodes, Node{Title: title.(string), Label: "movie"})
				movIdx := len(result.Nodes) - 1
				for _, actor := range actors.([]interface{}) {
					idx := -1
					for i, node := range result.Nodes {
						if actor == node.Title && node.Label == "actor" {
							idx = i
							break
						}
					}
					if idx == -1 {
						result.Nodes = append(result.Nodes, Node{Title: actor.(string), Label: "actor"})
						result.Links = append(result.Links, Link{Source: len(result.Nodes) - 1, Target: movIdx})
					} else {
						result.Links = append(result.Links, Link{Source: idx, Target: movIdx})
					}
				}
			}
			return result, nil
		})
		if err != nil {
			log.Println("error querying graph:", err)
			return
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
	fmt.Printf("Running on port %s, database is at %s\n", port, configuration.Url)
	panic(http.ListenAndServe(":"+port, httpgzip.NewHandler(serveMux)))
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
	if !strings.HasPrefix(lookupEnvOrGetDefault("NEO4J_VERSION", ""), "4") {
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

func unsafeClose(closeable io.Closer) {
	if err := closeable.Close(); err != nil {
		log.Fatal(fmt.Errorf("could not close resource: %w", err))
	}
}
