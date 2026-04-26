package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
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

	if err := initDB(db); err != nil {
		log.Fatal(err)
	}

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
func computeEMA(db *sql.DB, userID int) (float64, error) {
	rows, err := db.Query(`
		SELECT AVG(kg)
		FROM weight
		WHERE user_id = ? AND reported_at >= datetime('now', '-14 days')
		GROUP BY date(reported_at)
		ORDER BY date(reported_at) ASC`, userID)
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

		// guess which user is reporting based on most recent weight entries
		// for each user. if no recent entries, assign to null.
		userID, err := guessUserByWeight(db, kgInput)
		if err != nil {
			log.Printf("Failed to guess user: %v", err)
			http.Error(w, "Failed to guess user", http.StatusInternalServerError)
			return
		}

		trendArrow, err := computeUserTrendArrow(db, userID)
		if err != nil {
			log.Printf("Failed to compute trend: %v", err)
			trendArrow = "sideways"
		}

		_, err = db.Exec("INSERT INTO weight (kg, user_id) VALUES (?, ?)", kgInput, userID)
		if err != nil {
			log.Printf("Failed to save weight: %v", err)
			http.Error(w, "Failed to save weight", http.StatusInternalServerError)
			return
		}
		log.Printf("Weight %.2f kg saved", kgInput)

		ema, err := computeEMA(db, userID)
		if err != nil {
			log.Printf("Failed to compute EMA: %v", err)
			ema = kgInput
		}

		select {
		case eventChan <- event{
			WeightEMAlbs: ema * kgToLbs,
			TrendArrow:   trendArrow,
			Timestamp:    time.Now(),
		}:
		default:
			log.Println("Event channel full, skipping broadcast")
		}

		fmt.Fprintln(w, "Weight reported successfully")
	}
}

func initDB(db *sql.DB) error {
	createSQLs := []string{
		`CREATE TABLE IF NOT EXISTS user (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			email TEXT NOT NULL UNIQUE,
			latest_weight_kg REAL DEFAULT NULL,
			latest_reported_at DATETIME DEFAULT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS weight (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kg REAL NOT NULL,
			user_id INTEGER REFERENCES user(id) DEFAULT NULL,
			reported_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		// create a trigger to update user's latest weight and timestamp on new weight entry
		`CREATE TRIGGER IF NOT EXISTS update_user_latest_weight
		AFTER INSERT ON weight
		FOR EACH ROW
		BEGIN
			UPDATE user
			SET latest_weight_kg = NEW.kg,
				latest_reported_at = NEW.reported_at
			WHERE id = NEW.user_id;
		END;`,
	}
	for _, sql := range createSQLs {
		_, err := db.Exec(sql)
		if err != nil {
			return fmt.Errorf("failed to execute SQL: %w", err)
		}
	}

	if err := initDefaultUsers(db); err != nil {
		return fmt.Errorf("failed to initialize default users: %w", err)
	}

	return nil
}

// TODO this certainly isn't the way to do things. maybe inline the file?
const defaultUsersFile = "./default_users.json"

func initDefaultUsers(db *sql.DB) error {
	// get the file
	b, err := os.ReadFile(defaultUsersFile)
	if err != nil {
		return fmt.Errorf("failed to read default users file: %w", err)
	}

	type user struct {
		Name     string  `json:"name"`
		WeightKg float64 `json:"weight_kg"`
		Email    string  `json:"email"`
	}
	type userList struct {
		Users []user `json:"users"`
	}

	var ul userList
	if err := json.Unmarshal(b, &ul); err != nil {
		return fmt.Errorf("failed to unmarshal default users: %w", err)
	}
	for _, u := range ul.Users {
		_, err := db.Exec("INSERT OR IGNORE INTO user (name, email, latest_weight_kg, latest_reported_at) VALUES (?, ?, ?, ?)",
			u.Name, u.Email, u.WeightKg, time.Now())
		if err != nil {
			return fmt.Errorf("failed to insert default user: %w", err)
		}
	}
	return nil
}

func guessUserByWeight(db *sql.DB, kg float64) (int, error) {
	// TODO magic number -30 here has a unit but should be configurable.
	// only consider users with recent weight entries
	rows, err := db.Query(`
		SELECT id, latest_weight_kg FROM user 
		WHERE latest_weight_kg IS NOT NULL
		 AND latest_reported_at >= datetime('now', '-30 days')`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type userWeight struct {
		UserID int
		Kg     float64
	}
	var recentWeights []userWeight
	for rows.Next() {
		var uw userWeight
		if err := rows.Scan(&uw.UserID, &uw.Kg); err != nil {
			continue
		}
		recentWeights = append(recentWeights, uw)
	}

	if len(recentWeights) == 0 {
		return 0, nil // no recent weights, assign to null
	}

	// find the most recent weight entry within 10% of the input weight
	for _, uw := range recentWeights {
		if math.Abs(uw.Kg-kg) <= (kg * 0.10) { // within 10% of input weight
			return uw.UserID, nil
		}
	}

	return 0, nil // no close match, assign to null
}

func computeUserTrendArrow(db *sql.DB, userID int) (string, error) {
	rows, err := db.Query(`
		SELECT kg
		FROM weight
		WHERE user_id = ?
		AND reported_at >= datetime('now', '-14 days')
		ORDER BY reported_at ASC`, userID)
	if err != nil {
		return "sideways", err
	}
	defer rows.Close()

	var weights []float64
	for rows.Next() {
		var kg float64
		if err := rows.Scan(&kg); err != nil {
			continue
		}
		weights = append(weights, kg)
	}

	if len(weights) < 2 {
		return "sideways", nil // not enough data to determine trend
	}

	// simple trend analysis: compare first and last weight
	first := weights[0]
	last := weights[len(weights)-1]
	if last < first*0.95 {
		return "down", nil // more than 5% decrease
	} else if last < first {
		return "dropping", nil // some decrease
	} else if last > first*1.05 {
		return "up", nil // more than 5% increase
	} else if last > first {
		return "rising", nil // some increase
	}
	return "sideways", nil // no significant change
}
