package main

import (
	"html/template"
	"log"
	"net/http"
	"os"

	"github.com/mattkoler/board_game_helper/collection"
)

func main() {
	tpl, err := template.ParseGlob("resources/*.html")
	if err != nil {
		log.Fatalf("unable to parse html resources: %s", err)
	}

	http.HandleFunc("/", collection.Home(tpl))
	http.HandleFunc("/collection", collection.Collection(tpl, http.DefaultClient))

	port := os.Getenv("PORT")

	if port == "" {
		port = "8080"
		//log.Fatal("$PORT must be set")
	}

	log.Fatalf("serve failed: %s", http.ListenAndServe(":"+port, nil))
}
