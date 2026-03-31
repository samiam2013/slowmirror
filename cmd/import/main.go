package main

import (
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Entry struct {
	Date string  `json:"date"`
	Qty  float64 `json:"qty"`
}

type Metric struct {
	Name string  `json:"name"`
	Data []Entry `json:"data"`
}

type JSONFile struct {
	Data struct {
		Metrics []Metric `json:"metrics"`
	} `json:"data"`
}

const lbsToKg = 0.45359237

func main() {
	f, err := os.Open("weight_historical.json")
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()

	var payload JSONFile
	if err := json.NewDecoder(f).Decode(&payload); err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("sqlite3", "weight.db")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	var entries []Entry
	for _, m := range payload.Data.Metrics {
		if m.Name == "weight_body_mass" {
			entries = m.Data
			break
		}
	}

	stmt, err := db.Prepare("INSERT INTO weights (kg, reported_at) VALUES (?, ?)")
	if err != nil {
		log.Fatal(err)
	}
	defer stmt.Close()

	inserted := 0
	for _, e := range entries {
		t, err := time.Parse("2006-01-02 15:04:05 -0700", e.Date)
		if err != nil {
			log.Printf("skipping unparseable date %q: %v", e.Date, err)
			continue
		}
		kg := e.Qty * lbsToKg
		_, err = stmt.Exec(kg, t.UTC().Format("2006-01-02 15:04:05"))
		if err != nil {
			log.Printf("failed to insert %v: %v", t, err)
			continue
		}
		inserted++
	}
	log.Printf("Inserted %d/%d entries", inserted, len(entries))
}
