package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
)

type graphqlResponse[T any] struct {
	Data   T              `json:"data"`
	Errors []graphqlError `json:"errors,omitempty"`
}

type graphqlError struct {
	Message string `json:"message"`
}

type graphqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

func graphqlQuery[T any](ctx context.Context, client *http.Client, endpoint string, query string, variables map[string]any) (graphqlResponse[T], error) {
	request := graphqlRequest{
		Query:     query,
		Variables: variables,
	}

	buf := new(bytes.Buffer)
	if err := json.NewEncoder(buf).Encode(request); err != nil {
		return graphqlResponse[T]{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, buf)
	if err != nil {
		return graphqlResponse[T]{}, err
	}

	req.Header.Set("Content-Type", "application/json")
	r, err := client.Do(req) //nolint:gosec // endpoint is an internal constant
	if err != nil {
		return graphqlResponse[T]{}, err
	}
	defer func() {
		_ = r.Body.Close()
	}()

	var response graphqlResponse[T]
	if err := json.NewDecoder(r.Body).Decode(&response); err != nil {
		return graphqlResponse[T]{}, err
	}

	return response, nil
}
