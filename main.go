package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"strconv"

	_ "github.com/mattn/go-sqlite3"
)

const kgToLbs = 2.20462

func main() {
	db, err := sql.Open("sqlite3", "weight.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	initDB(db)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		view(w, r, db)
	})
	mux.HandleFunc("POST /report", func(w http.ResponseWriter, r *http.Request) {
		report(w, r, db)
	})

	log.Println("Starting server")
	http.ListenAndServe(":8080", mux)
}

func view(w http.ResponseWriter, r *http.Request, db *sql.DB) {
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

func report(w http.ResponseWriter, r *http.Request, db *sql.DB) {
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
	fmt.Fprintln(w, "Weight reported successfully")
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
