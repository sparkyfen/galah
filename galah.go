package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/0x4d31/galah/enrich"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/sync/errgroup"
)

type Event struct {
	Timestamp    time.Time    `json:"timestamp"`
	SrcIP        string       `json:"srcIP"`
	SrcHost      string       `json:"srcHost"`
	Tags         []string     `json:"tags"`
	SrcPort      string       `json:"srcPort"`
	SensorName   string       `json:"sensorName"`
	Port         string       `json:"port"`
	HTTPRequest  HTTPRequest  `json:"httpRequest"`
	HTTPResponse HTTPResponse `json:"httpResponse"`
	// TODO: Sessionize the incoming requests based on the sessionTTL and source IP.
	// SessionID    string       `json:"sessionID"`
}

type HTTPRequest struct {
	Method              string `json:"method"`
	ProtocolVersion     string `json:"protocolVersion"`
	Request             string `json:"request"`
	UserAgent           string `json:"userAgent"`
	Headers             string `json:"headers"`
	HeadersSorted       string `json:"headersSorted"`
	HeadersSortedSha256 string `json:"headersSortedSha256"`
	Body                string `json:"body"`
	BodySha256          string `json:"bodySha256"`
}

type HTTPResponse struct {
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type Args struct {
	Interface  string
	ConfigFile string
	DbPath     string
	OutputFile string
	Verbose    bool
}

type App struct {
	Config      *Config
	DB          *sql.DB
	OutputFile  string
	Verbose     bool
	Servers     map[uint16]*http.Server
	Hostname    string
	EnrichCache *enrich.Default
}

var ignoreHeaders = map[string]bool{
	// Standard headers to ignore
	"content-length": true,
	"content-type":   true,
	"date":           true,
	"expires":        true,
	"last-modified":  true,
	// OpenAI made-up headers to ignore
	"http":     true,
	"http/1.0": true,
	"http/1.1": true,
	"http/1.2": true,
	"http/2.0": true,
}

const (
	version   = "1.0"
	cacheSize = 1_000_000
	lookupTTL = 1 * time.Hour
	// sessionTTL = 2 * time.Minute
)

func printBanner() {
	banner := `
 ██████   █████  ██       █████  ██   ██ 
██       ██   ██ ██      ██   ██ ██   ██ 
██   ███ ███████ ██      ███████ ███████ 
██    ██ ██   ██ ██      ██   ██ ██   ██ 
 ██████  ██   ██ ███████ ██   ██ ██   ██ 
  llm-based web honeypot // version %s
       author: Adel "0x4D31" Karimi

`
	fmt.Printf(banner, version)
	return
}

// Main function
func main() {
	printBanner()
	args := parseArgs()

	config, err := LoadConfig(args.ConfigFile)
	if err != nil {
		log.Fatalf("Error loading config: %v", err)
	}

	db := initDB(args.DbPath)
	defer db.Close()

	hostname, err := getHostname()
	if err != nil {
		log.Fatalf("Error getting hostname: %v", err)
	}

	enrichCache := enrich.New(&enrich.Config{
		CacheSize: cacheSize,
		CacheTTL:  lookupTTL,
	})

	app := &App{
		Config:      config,
		DB:          db,
		OutputFile:  args.OutputFile,
		Verbose:     args.Verbose,
		Hostname:    hostname,
		EnrichCache: enrichCache,
	}

	app.ListenForShutdownSignals()

	err = app.startServers()
	if err != nil {
		log.Println(err)
	}
}

func parseArgs() *Args {
	args := &Args{}
	flag.StringVar(&args.Interface, "i", "", "interface to serve on")
	flag.StringVar(&args.ConfigFile, "c", "config.yaml", "path to config file")
	flag.StringVar(&args.DbPath, "db", "cache.db", "path to database file")
	flag.StringVar(&args.OutputFile, "o", "log.json", "path to output log file")
	flag.BoolVar(&args.Verbose, "v", false, "verbose mode")

	flag.Parse()

	// Set default interface to first non-loopback interface
	if args.Interface == "" {
		interfaceName, err := getDefaultInterface()
		if err != nil {
			log.Fatalf("Error getting default interface: %v", err)
		}
		args.Interface = interfaceName
	}

	return args
}

func initDB(dbPath string) *sql.DB {
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return nil
	}

	_, err = db.Exec(`
    CREATE TABLE IF NOT EXISTS cache (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		cachedAt DATETIME,
		key TEXT,
		response TEXT
	)	
`)
	if err != nil {
		log.Fatalf("Error creating table: %v", err)
	}

	return db
}

func (app *App) startServers() error {
	var g errgroup.Group

	for _, pc := range app.Config.Ports {
		pc := pc // Capture the loop variable
		g.Go(func() error {
			server := app.setupServer(pc)
			app.Servers = make(map[uint16]*http.Server)

			var err error
			switch pc.Protocol {
			case "TLS":
				err = app.startTLSServer(server, pc)
			case "HTTP":
				err = app.startHTTPServer(server, pc)
			default:
				err = fmt.Errorf("Unknown protocol for port %d", pc.Port)
			}
			if err != nil {
				return err
			}

			return nil
		})
	}

	return g.Wait()
}

func (app *App) setupServer(pc PortConfig) *http.Server {
	serverAddr := fmt.Sprintf(":%d", pc.Port)
	server := &http.Server{
		Addr: serverAddr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			app.handleRequest(w, r, serverAddr)
		}),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	return server
}

func (app *App) startTLSServer(server *http.Server, pc PortConfig) error {
	if pc.TLSProfile == "" {
		return fmt.Errorf("Error: TLS profile not configured for port %d", pc.Port)
	}

	tlsConfig, ok := app.Config.TLS[pc.TLSProfile]
	if !ok || tlsConfig.Certificate == "" || tlsConfig.Key == "" {
		return fmt.Errorf("Error: TLS profile incomplete for port %d", pc.Port)
	}

	log.Printf("Starting HTTPS server on port %d with TLS profile: %s", pc.Port, pc.TLSProfile)
	err := server.ListenAndServeTLS(tlsConfig.Certificate, tlsConfig.Key)
	if err != nil {
		return fmt.Errorf("Error starting HTTPS server on port %d: %v", pc.Port, err)
	}
	return nil
}

func (app *App) startHTTPServer(server *http.Server, pc PortConfig) error {
	log.Printf("Starting HTTP server on port %d", pc.Port)
	err := server.ListenAndServe()
	if err != nil {
		return fmt.Errorf("Error starting HTTP server on port %d: %v", pc.Port, err)
	}
	return nil
}

func (app *App) handleRequest(w http.ResponseWriter, r *http.Request, serverAddr string) {
	_, port, err := net.SplitHostPort(serverAddr)
	if err != nil {
		port = ""
	}

	if app.Verbose {
		log.Printf("Received a request for %q from %s", r.URL.String(), r.RemoteAddr)
	}

	response, err := app.checkDB(r, port)
	if err != nil {
		if app.Verbose {
			log.Printf("Request cache miss for %q: %s", r.URL.String(), err)
		}

		response, err = app.generateAndCacheResponse(r, port)
		if err != nil {
			log.Println("Error generating response:", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	// Parse the JSON-encoded data into a HTTPResponse struct, and send it to the client.
	var respData HTTPResponse
	if err := json.Unmarshal(response, &respData); err != nil {
		log.Println("Error unmarshalling the json-encoded data:", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if app.Verbose {
		log.Println("Sending the crafted response to", r.RemoteAddr)
	}
	sendResponse(w, respData)

	// The response headers are logged exactly as generated by Perplexity AI, however,
	// certain headers are excluded before sending the response to the client.
	event := app.makeEvent(r, respData, port)
	app.writeLog(event)
}

func (app *App) makeEvent(req *http.Request, resp HTTPResponse, port string) Event {
	var tags []string

	srcIP, srcPort, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		srcIP = req.RemoteAddr
		srcPort = ""
	}

	e := app.EnrichCache
	srcIPInfo, err := e.Process(srcIP)
	if err != nil {
		log.Printf("Error getting enrichment info for %q: %s", srcIP, err)
	}
	if s := srcIPInfo.KnownScanner; s != "" {
		tags = append(tags, s)
	}

	httpRequest := extractHTTPRequestInfo(req)
	return Event{
		Timestamp:    time.Now(),
		SrcIP:        srcIP,
		SrcHost:      srcIPInfo.Host,
		SrcPort:      srcPort,
		Tags:         tags,
		SensorName:   app.Hostname,
		Port:         port,
		HTTPRequest:  httpRequest,
		HTTPResponse: resp,
	}
}

func extractHTTPRequestInfo(r *http.Request) HTTPRequest {
	httpRequest := HTTPRequest{}
	httpRequest.Method = r.Method
	httpRequest.ProtocolVersion = r.Proto
	httpRequest.Request = r.RequestURI
	httpRequest.UserAgent = r.UserAgent()
	httpRequest.Headers = extractHeaderValues(r.Header)
	headerKeys := extractHeaderKeys(r.Header)
	sort.Strings(headerKeys)
	httpRequest.HeadersSorted = strings.Join(headerKeys, ",")
	httpRequest.HeadersSortedSha256 = calculateHeadersSortedSha256(headerKeys)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		log.Println("Error reading request body:", err)
	}
	httpRequest.Body = string(bodyBytes)
	httpRequest.BodySha256 = func(data []byte) string {
		hash := sha256.Sum256(data)
		return hex.EncodeToString(hash[:])
	}(bodyBytes)

	return httpRequest
}

func extractHeaderKeys(headers http.Header) []string {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	return keys
}

func extractHeaderValues(headers http.Header) string {
	values := make([]string, 0, len(headers))
	for key, value := range headers {
		values = append(values, fmt.Sprintf("%s: %v", key, value))
	}
	return strings.Join(values, ", ")
}

func calculateHeadersSortedSha256(headerKeys []string) string {
	hash := sha256.Sum256([]byte(strings.Join(headerKeys, ",")))
	return hex.EncodeToString(hash[:])
}

func (app *App) checkDB(r *http.Request, port string) ([]byte, error) {
	cacheKey := getDBKey(r, port)
	var response []byte
	var cachedAt time.Time

	// Order by cachedAt DESC to get the most recent record.
	row := app.DB.QueryRow("SELECT cachedAt, response FROM cache WHERE key = ? ORDER BY cachedAt DESC LIMIT 1", cacheKey)

	err := row.Scan(&cachedAt, &response)
	if err == sql.ErrNoRows {
		return nil, errors.New("Not found in cache")
	}
	// TODO: Add an option to disable caching or set an indefinite caching (no expiration).
	if time.Since(cachedAt) > time.Duration(app.Config.CacheDuration)*time.Hour {
		return nil, errors.New("Cached record is too old")
	}

	return response, err
}

func getDBKey(r *http.Request, port string) string {
	return port + "_" + r.URL.String()
}

func (app *App) generateAndCacheResponse(r *http.Request, port string) ([]byte, error) {
	responseString, err := GeneratePerplexityAIResponse(app.Config, r)
	if err != nil {
		log.Print(err)
		return nil, err
	}
	if app.Verbose {
		log.Println("Generated HTTP response:", responseString)
	}

	responseBytes := []byte(responseString)
	DBKey := getDBKey(r, port)
	currentTime := time.Now()
	_, err = app.DB.Exec("INSERT OR REPLACE INTO cache (cachedAt, key, response) VALUES (?, ?, ?)", currentTime, DBKey, responseBytes)

	return responseBytes, err
}

func sendResponse(w http.ResponseWriter, response HTTPResponse) {

	for key, value := range response.Headers {
		if !isExcludedHeader(key) {
			w.Header().Set(key, value)
		}
	}

	_, err := w.Write([]byte(response.Body))
	if err != nil {
		log.Println("Error writing response:", err)
	}
}

func isExcludedHeader(headerKey string) bool {
	return ignoreHeaders[strings.ToLower(headerKey)]
}

func (app *App) writeLog(event Event) {
	f, err := os.OpenFile(app.OutputFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Error opening log file: %v", err)
		return
	}
	defer f.Close()

	eventJSON, err := json.Marshal(event)
	if err != nil {
		log.Printf("Error marshaling event to JSON: %v", err)
		return
	}

	if _, err = f.Write(append(eventJSON, '\n')); err != nil {
		log.Printf("Error writing to log file: %v", err)
		return
	}
}

func getDefaultInterface() (string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagLoopback == 0 && iface.Flags&net.FlagUp != 0 {
			return iface.Name, nil
		}
	}
	return "", errors.New("No active non-loopback interface found")
}

func getHostname() (string, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return "", fmt.Errorf("failed to get hostname: %v", err)
	}
	return hostname, nil
}

func (app *App) ListenForShutdownSignals() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sig
		log.Println("Received shutdown signal. Shutting down servers...")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		for _, server := range app.Servers {
			if err := server.Shutdown(ctx); err != nil {
				log.Printf("Error shutting down server: %v", err)
			}
		}

		log.Println("All servers shut down gracefully.")
		os.Exit(0)
	}()
}
