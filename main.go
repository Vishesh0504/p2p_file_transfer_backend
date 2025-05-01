package main

import (
	"net/http"
	"p2p_file_transfer/handlers"
	"p2p_file_transfer/middleware"

	"github.com/gorilla/mux"
	"github.com/rs/cors"
)

func main() {
	r := mux.NewRouter()
	r.Use(middleware.LoggingMiddleware)
	r.HandleFunc("/fileMetadata", handlers.MetadataHandler).Methods("POST")
	r.HandleFunc("/signalSDP", handlers.SDPHandler).Methods("POST")

	// Create CORS handler with appropriate settings for WebRTC
	c := cors.New(cors.Options{
		AllowedOrigins: []string{"http://localhost:3000","http://192.168.38.204:3000"},
		AllowedMethods: []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders: []string{
			"Content-Type",
			"Authorization",
			"Access-Control-Allow-Origin",
			"Access-Control-Allow-Headers",
			"Accept",
		},
		AllowCredentials: true,
		// Add debug mode if you need to troubleshoot CORS issues
		Debug: false,
	})

	// Apply CORS middleware to router
	handler := c.Handler(r)

	http.ListenAndServe(":8080", handler)
}