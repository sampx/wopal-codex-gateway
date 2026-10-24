package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wopal-codex-gateway/internal/config"
)

var wopalModelsHTTPClient = &http.Client{Timeout: 30 * time.Second}

// FetchWopalModels lists the model ids exposed by the Wopal AI /models endpoint.
func FetchWopalModels(baseURL, apiKey string) ([]string, error) {
	snapshot, err := FetchWopalModelMetadata(baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(snapshot.Models))
	for _, model := range snapshot.Models {
		ids = append(ids, model.Name)
	}
	return ids, nil
}

// maxModelMetadataBytes bounds the provider response before decoding it.
const maxModelMetadataBytes = 4 * 1024 * 1024

// FetchWopalModelMetadata reads only explicit capabilities from /models. It
// never changes user configuration or the last successfully saved snapshot.
func FetchWopalModelMetadata(baseURL, apiKey string) (*ModelMetadataSnapshot, error) {
	baseURL = normalizeMetadataBaseURL(baseURL)
	if err := validateMetadataBaseURL(baseURL); err != nil {
		return nil, err
	}
	endpoint := baseURL + "/models"
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

	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("Wopal AI validation failed: read /models response: %w", err)
	}

	if len(body) > maxModelMetadataBytes {
		return nil, fmt.Errorf("Wopal AI validation failed: /models response exceeds %d byte limit", maxModelMetadataBytes)
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

	models := make([]config.ExternalModelConfig, 0, len(*data))
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
			model := config.ExternalModelConfig{Name: id}
			model.ContextWindow = positiveMetadataInt(item["context_window"])
			if model.ContextWindow == nil {
				model.ContextWindow = positiveMetadataInt(item["contextWindow"])
			}
			model.Vision = metadataBool(item["vision"])
			if model.Vision == nil {
				model.Vision = metadataModalitiesVision(item["input_modalities"])
			}
			model.Reasoning = metadataBool(item["reasoning"])
			models = append(models, model)
		}
	}
	return &ModelMetadataSnapshot{BaseURL: baseURL, FetchedAt: time.Now().UTC(), Models: models}, nil
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

func positiveMetadataInt(raw json.RawMessage) *int {
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value <= 0 {
		return nil
	}
	return &value
}

func metadataBool(raw json.RawMessage) *bool {
	var value *bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil
	}
	return value
}

// Only a nonempty list consisting entirely of recognized text/image modalities
// is evidence. Unknown or malformed elements leave vision unspecified.
func metadataModalitiesVision(raw json.RawMessage) *bool {
	var modalities []json.RawMessage
	if err := json.Unmarshal(raw, &modalities); err != nil || len(modalities) == 0 {
		return nil
	}
	vision := false
	for _, rawModality := range modalities {
		var modality string
		if err := json.Unmarshal(rawModality, &modality); err != nil {
			return nil
		}
		switch modality {
		case "text":
		case "image":
			vision = true
		default:
			return nil
		}
	}
	return &vision
}
