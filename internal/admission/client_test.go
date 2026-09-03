package admission

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientAdmissionResponsesAndToken(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	status := http.StatusNoContent
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization = %q", got)
		}
		writer.WriteHeader(status)
	}))
	defer server.Close()
	client, err := NewClient(server.URL+"/admit", token)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	allowed, err := client.Admit(context.Background())
	if err != nil || !allowed {
		t.Fatalf("Admit() = %v, %v", allowed, err)
	}
	status = http.StatusTooManyRequests
	allowed, err = client.Admit(context.Background())
	if err != nil || allowed {
		t.Fatalf("Admit() = %v, %v", allowed, err)
	}
	status = http.StatusInternalServerError
	if _, err := client.Admit(context.Background()); err == nil {
		t.Fatal("Admit() error = nil")
	}
}

func TestNewClientRejectsInvalidConfiguration(t *testing.T) {
	for _, test := range []struct{ endpoint, token string }{
		{endpoint: "relative", token: "0123456789abcdef0123456789abcdef"},
		{endpoint: "https://example.com", token: "short"},
	} {
		if _, err := NewClient(test.endpoint, test.token); err == nil {
			t.Fatal("NewClient() error = nil")
		}
	}
}
