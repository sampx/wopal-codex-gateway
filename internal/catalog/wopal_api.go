package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var wopalModelsHTTPClient = &http.Client{Timeout: 30 * time.Second}

// FetchWopalModels lists the model ids exposed by the Wopal AI /models endpoint.
func FetchWopalModels(baseURL, apiKey string) ([]string, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/models"
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("Wopal AI validation failed: create /models request: %w", err)
	}
	request.Header.Set("accept", "application/json")
	request.Header.Set("authorization", "Bearer "+apiKey)

	response, err := wopalModelsHTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Wopal AI validation failed: request /models: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("Wopal AI validation failed: read /models response: %w", err)
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf(
			"Wopal AI validation failed: HTTP %d%s",
			response.StatusCode,
			wopalAPIErrorDetail(body),
		)
	}

	var validJSON json.RawMessage
	if err := json.Unmarshal(body, &validJSON); err != nil {
		return nil, fmt.Errorf("Wopal AI validation failed: /models did not return JSON")
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(validJSON, &payload); err != nil {
		return nil, fmt.Errorf("Wopal AI validation failed: /models response is missing data[]")
	}
	rawData, ok := payload["data"]
	if !ok {
		return nil, fmt.Errorf("Wopal AI validation failed: /models response is missing data[]")
	}
	var data *[]json.RawMessage
	if err := json.Unmarshal(rawData, &data); err != nil || data == nil {
		return nil, fmt.Errorf("Wopal AI validation failed: /models response is missing data[]")
	}

	models := make([]string, 0, len(*data))
	for _, rawItem := range *data {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &item); err != nil {
			continue
		}
		rawID, ok := item["id"]
		if !ok {
			continue
		}
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil {
			continue
		}
		id = strings.TrimSpace(id)
		if id != "" {
			models = append(models, id)
		}
	}
	return models, nil
}

func wopalAPIErrorDetail(body []byte) string {
	if strings.TrimSpace(string(body)) == "" {
		return ""
	}
	var payload struct {
		Error struct {
			Message any `json:"message"`
		} `json:"error"`
		Message any `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}

	message, ok := payload.Error.Message.(string)
	if !ok || strings.TrimSpace(message) == "" {
		message, ok = payload.Message.(string)
	}
	if !ok || strings.TrimSpace(message) == "" {
		return ""
	}
	return ": " + strings.TrimSpace(message)
}
