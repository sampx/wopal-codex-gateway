package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestWopalAPIFetchModelsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %s, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":" alpha "},{"id":""},{"id":7},{"id":"beta"},{"id":"alpha"}]}`))
	}))
	defer server.Close()

	got, err := fetchWopalModels(server.URL+"/v1///", "test-key")
	if err != nil {
		t.Fatalf("fetchWopalModels() error = %v", err)
	}
	want := []string{"alpha", "beta", "alpha"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fetchWopalModels() = %#v, want %#v", got, want)
	}
}

func TestWopalAPIFetchModelsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":" invalid key "}}`))
	}))
	defer server.Close()

	_, err := fetchWopalModels(server.URL, "test-key")
	if err == nil || err.Error() != "Wopal AI validation failed: HTTP 401: invalid key" {
		t.Fatalf("fetchWopalModels() error = %v", err)
	}
}

func TestWopalAPIFetchModelsNonJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer server.Close()

	_, err := fetchWopalModels(server.URL, "test-key")
	if err == nil || err.Error() != "Wopal AI validation failed: /models did not return JSON" {
		t.Fatalf("fetchWopalModels() error = %v", err)
	}
}

func TestWopalAPIFetchModelsMissingData(t *testing.T) {
	for name, body := range map[string]string{
		"object without data": `{"object":"list"}`,
		"top-level array":     `[{"id":"a"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			_, err := fetchWopalModels(server.URL, "test-key")
			if err == nil || err.Error() != "Wopal AI validation failed: /models response is missing data[]" {
				t.Fatalf("fetchWopalModels() error = %v", err)
			}
		})
	}
}
