package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const kgToLbs = 2.20462

func main() {
	log.Println("Starting weight tracking server")
	log.Println("Connecting to database")
	db, err := sql.Open("sqlite3", "weight.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close() // error ignored

	log.Println("Database connection established, initializing database")
	initDB(db)

	log.Println("Database initialized, building routes")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", dashboard())
	eventsChan := make(chan event)
	// Start the event broadcaster in a separate goroutine
	go eventBroadcast(eventsChan, subscribers)
	mux.HandleFunc("POST /report", report(db, eventsChan))
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		subscriptionChan := subscribeToEvents()
		eventSubscribe(subscriptionChan)(w, r)
	})
	mux.HandleFunc("GET /view", view(db))

	log.Println("Starting server")
	http.ListenAndServe(":8080", mux)
}

var subsBacking = make([]chan event, 0)
var subscribers = &subsBacking

func subscribeToEvents() chan event {
	subscriptionChan := make(chan event)
	new := append(*subscribers, subscriptionChan)
	*subscribers = new
	log.Printf("New subscriber added, total subscribers: %d", len(*subscribers))
	return subscriptionChan
}

func eventBroadcast(eventChan chan event, subscriptionList *[]chan event) {
	for {
		log.Println("Waiting for new event to broadcast")
		e := <-eventChan
		for _, sub := range *subscriptionList {
			log.Println("Broadcasting event to subscriber")
			sub <- e
		}
		log.Println("Event broadcast complete")
	}
}

func dashboard() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile("./dashboard.html")
		if err != nil {
			log.Printf("Failed to read dashboard file: %v", err)
			http.Error(w, "Failed to load dashboard", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write(b)
	}
}

type event struct {
	WeightEMAlbs float64   `json:"ema"`
	TrendArrow   string    `json:"trend"`
	Timestamp    time.Time `json:"timestamp"`
}

func eventSubscribe(eventSubscription chan event) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// This is a placeholder for the SSE endpoint that will stream weight events to the dashboard
		// In a real implementation, this would listen for new weight entries and calculate the EMA and trend arrow
		// For now, it just sends a dummy event every 5 seconds
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.(http.Flusher).Flush()

		for {
			select {
			case e := <-eventSubscription:
				log.Println("Sending event to subscriber")
				fmt.Fprintf(w, "data: {\"ema\": %.1f, \"trend\": \"%s\", \"timestamp\": \"%s\"}\n\n", e.WeightEMAlbs, e.TrendArrow, e.Timestamp.Format(time.RFC3339))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			case <-r.Context().Done():
				log.Println("SSE connection closed")
				return
			}
		}
	}
}

func view(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// get the last 14 days of weight data from the database and display it
		rows, err := db.Query("SELECT kg, reported_at FROM weights WHERE reported_at >= datetime('now', '-14 days') ORDER BY reported_at ASC")
		if err != nil {
			log.Printf("Failed to query weights: %v", err)
			http.Error(w, "Failed to query weights", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		results := []struct {
			kg         float64
			ema        float64
			reportedAt string
		}{}

		const alpha = 0.2
		ema := 0.0
		weightCount := 0
		for rows.Next() {
			var kg float64
			var reportedAt string
			if err := rows.Scan(&kg, &reportedAt); err != nil {
				log.Printf("Failed to scan row: %v", err)
				continue
			}

			if weightCount == 0 {
				ema = kg
			} else {
				ema = alpha*kg + (1-alpha)*ema
			}
			weightCount++

			results = append(results, struct {
				kg         float64
				ema        float64
				reportedAt string
			}{kg, ema, reportedAt})
		}

		if len(results) == 0 {
			fmt.Fprintln(w, "No weight data reported in the last 14 days.")
			return
		}

		emaLbs := ema * kgToLbs
		fmt.Fprintf(w, "EMA of weight over the last 14 days: %.1f lbs (%.1f kg)\n\n\n\n", emaLbs, ema)

		fmt.Fprintln(w, "Weight data for the last 14 days:")
		for _, r := range results {
			lbs := r.kg * kgToLbs
			emaLbs := r.ema * kgToLbs
			fmt.Fprintf(w, "EMA: %.1f lbs (%.1f kg), Weight: %.1f lbs (%.1f kg), Reported at: %s\n", emaLbs, r.ema, lbs, r.kg, r.reportedAt)
		}
		log.Println("View endpoint accessed")
	}
}

func report(db *sql.DB, eventChan chan event) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kgInputRaw := r.FormValue("kg")
		kgInput, err := strconv.ParseFloat(kgInputRaw, 32)
		if err != nil {
			log.Printf("Invalid weight input: %v", err)
			http.Error(w, "Invalid weight input", http.StatusBadRequest)
			return
		}
		log.Printf("Received weight: %.2f kg", kgInput)
		_, err = db.Exec("INSERT INTO weights (kg) VALUES (?)", kgInput)
		if err != nil {
			log.Printf("Failed to save weight: %v", err)
			http.Error(w, "Failed to save weight", http.StatusInternalServerError)
			return
		}
		log.Printf("Weight %.2f kg saved successfully", kgInput)

		eventChan <- event{
			WeightEMAlbs: kgInput * kgToLbs, // In a real implementation, this would be the calculated EMA value
			TrendArrow:   "sideways",
			Timestamp:    time.Now(),
		}
		log.Printf("Event sent for weight: %.2f kg", kgInput)
		fmt.Fprintln(w, "Weight reported successfully")
	}
}

func initDB(db *sql.DB) {
	createTableSQL := `CREATE TABLE IF NOT EXISTS weights (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kg REAL NOT NULL,
		reported_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);`
	_, err := db.Exec(createTableSQL)
	if err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}
}
