package api

import "net/http"

func NewRouter() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", health)
	mux.HandleFunc("GET /ready", ready)

	mux.HandleFunc("POST /v1/auth/register", register)
	mux.HandleFunc("POST /v1/auth/login", login)

	mux.HandleFunc("POST /v1/createroom", createroom)
	mux.HandleFunc("GET /v1/listrooms", listrooms)

	return mux
}
