package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const (
	kgToLbs  = 2.20462
	emaAlpha = 0.2
)

func main() {
	log.Println("Starting weight tracking server")
	db, err := sql.Open("sqlite3", "weight.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	initDB(db)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", dashboard())
	eventsChan := make(chan event, 1)
	go eventBroadcast(eventsChan)
	mux.HandleFunc("POST /report", report(db, eventsChan))
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		ch := subscribeToEvents()
		defer unsubscribeFromEvents(ch)
		eventSubscribe(ch)(w, r)
	})

	log.Println("Listening on :8080")
	http.ListenAndServe(":8080", mux)
}

var (
	subsMu      sync.Mutex
	subscribers []chan event
)

func subscribeToEvents() chan event {
	ch := make(chan event, 1)
	subsMu.Lock()
	subscribers = append(subscribers, ch)
	log.Printf("Subscriber added, total: %d", len(subscribers))
	subsMu.Unlock()
	return ch
}

func unsubscribeFromEvents(ch chan event) {
	subsMu.Lock()
	for i, sub := range subscribers {
		if sub == ch {
			subscribers = append(subscribers[:i], subscribers[i+1:]...)
			break
		}
	}
	log.Printf("Subscriber removed, total: %d", len(subscribers))
	subsMu.Unlock()
}

func eventBroadcast(eventChan chan event) {
	for e := range eventChan {
		subsMu.Lock()
		subs := make([]chan event, len(subscribers))
		copy(subs, subscribers)
		subsMu.Unlock()
		for _, sub := range subs {
			select {
			case sub <- e:
			default:
				log.Println("Subscriber too slow, dropping event")
			}
		}
	}
}

func dashboard() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile("./dashboard.html")
		if err != nil {
			log.Printf("Failed to read dashboard: %v", err)
			http.Error(w, "Failed to load dashboard", http.StatusInternalServerError)
			return
		}
		w.Write(b)
	}
}

type event struct {
	WeightEMAlbs float64   `json:"ema"`
	TrendArrow   string    `json:"trend"`
	Timestamp    time.Time `json:"timestamp"`
}

func eventSubscribe(eventSubscription chan event) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.(http.Flusher).Flush()

		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case e := <-eventSubscription:
				render, err := json.Marshal(e)
				if err != nil {
					log.Printf("Failed to marshal event: %v", err)
					continue
				}
				fmt.Fprintf(w, "data: %s\n\n", render)
				w.(http.Flusher).Flush()
			case <-ticker.C:
				fmt.Fprintf(w, ": keepalive\n\n")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				log.Println("SSE client disconnected")
				return
			}
		}
	}
}

// computeEMA returns the exponential moving average of daily average weights over the last 14 days.
func computeEMA(db *sql.DB) (float64, error) {
	rows, err := db.Query(`
		SELECT AVG(kg)
		FROM weights
		WHERE reported_at >= datetime('now', '-14 days')
		GROUP BY date(reported_at)
		ORDER BY date(reported_at) ASC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var ema float64
	var count int
	for rows.Next() {
		var dailyAvg float64
		if err := rows.Scan(&dailyAvg); err != nil {
			continue
		}
		if count == 0 {
			ema = dailyAvg
		} else {
			ema = emaAlpha*dailyAvg + (1-emaAlpha)*ema
		}
		count++
	}
	return ema, rows.Err()
}

// computeTrendArrow returns "up", "rising", "sideways", "falling", or "down" based on the weekly average of 21 days
// if the average is less than 0.5 lb per week, it's "sideways".
// If it's between 0.5 and 2 lb per week, it's "rising" or "falling".
// If it's more than 2 lb per week, it's "up" or "down".
func computeTrendArrow(db *sql.DB) (string, error) {
	rows, err := db.Query(`
		SELECT AVG(kg)
		FROM weights
		WHERE reported_at >= datetime('now', '-21 days')
		GROUP BY date(reported_at)
		ORDER BY date(reported_at) ASC`)
	if err != nil {
		return "sideways", err
	}
	defer rows.Close()

	var weights []float64
	for rows.Next() {
		var dailyAvg float64
		if err := rows.Scan(&dailyAvg); err != nil {
			continue
		}
		weights = append(weights, dailyAvg)
	}
	if len(weights) < 2 {
		return "sideways", nil
	}

	first := weights[0]
	last := weights[len(weights)-1]
	deltaKg := last - first
	deltaLbs := deltaKg * kgToLbs
	deltaPerWeek := deltaLbs / (float64(len(weights)-1) / 7)

	lowerThreshold := 0.25
	upperThreshold := 1.0
	switch {
	case deltaPerWeek > upperThreshold:
		return "up", nil
	case deltaPerWeek > lowerThreshold:
		return "rising", nil
	case deltaPerWeek < -upperThreshold:
		return "down", nil
	case deltaPerWeek < -lowerThreshold:
		return "falling", nil
	default:
		return "sideways", nil
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
		_, err = db.Exec("INSERT INTO weights (kg) VALUES (?)", kgInput)
		if err != nil {
			log.Printf("Failed to save weight: %v", err)
			http.Error(w, "Failed to save weight", http.StatusInternalServerError)
			return
		}
		log.Printf("Weight %.2f kg saved", kgInput)

		ema, err := computeEMA(db)
		if err != nil {
			log.Printf("Failed to compute EMA: %v", err)
			ema = kgInput
		}

		trendStr, err := computeTrendArrow(db)
		if err != nil {
			log.Printf("Failed to compute trend: %v", err)
			trendStr = "sideways"
		}
		select {
		case eventChan <- event{
			WeightEMAlbs: ema * kgToLbs,
			TrendArrow:   trendStr,
			Timestamp:    time.Now(),
		}:
		default:
			log.Println("Event channel full, skipping broadcast")
		}

		fmt.Fprintln(w, "Weight reported successfully")
	}
}

func initDB(db *sql.DB) {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS weights (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		kg REAL NOT NULL,
		reported_at DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
	if err != nil {
		log.Fatalf("Failed to create table: %v", err)
	}
}
