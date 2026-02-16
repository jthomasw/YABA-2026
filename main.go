package main

import (
	"html/template"
	"log"
	"net/http"
)

// Struct to send data to HTML
type PageData struct {
	Title   string
	Message string
}

func main() {
	// Route handler
	http.HandleFunc("/", homeHandler)

	log.Println("Server running at http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	// Parse the HTML file
	tmpl := template.Must(template.ParseFiles("index.html"))

	// Data to send to HTML
	data := PageData{
		Title:   "Go + HTML Connected",
		Message: "This text is coming from Go!",
	}

	// Inject data into HTML
	tmpl.Execute(w, data)
}
