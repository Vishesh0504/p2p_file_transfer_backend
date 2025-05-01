package utils

import (
	"encoding/json"
	"net/http"
)

type ErrorResponse struct{
	Error string `json:"error"`
}

func RespondWithError(w http.ResponseWriter,code int,message string){
	RespondWithJSON(w,code,ErrorResponse{message})
}

func RespondWithJSON(w http.ResponseWriter,code int, payload interface{}){
	response,err := json.Marshal(payload)
	if err!=nil{
		http.Error(w,"internal server error",http.StatusInternalServerError)
	}
	w.Header().Set("Content-Type","application/json")
	w.WriteHeader(code)
	w.Write(response)
}