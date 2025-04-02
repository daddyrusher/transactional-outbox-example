package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateOrderHandler(t *testing.T) {
	order := Order{
		Status: "test",
	}
	body, _ := json.Marshal(order)
	req, err := http.NewRequest("POST", "/order", bytes.NewBuffer(body))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(createOrderHandler)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}
	var resp Order
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "test" {
		t.Errorf("expected order status 'test', got %s", resp.Status)
	}
}
