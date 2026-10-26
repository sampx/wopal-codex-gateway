package catalog

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// FetchOpenAIModelIDs reads an OpenAI-style {data:[{id:...}]} model list.
// Credentials are added only when the provider's modelsAuth resolves to
// provider-key; public discovery endpoints receive no Authorization header.
func FetchOpenAIModelIDs(modelsURL, apiKey string, useProviderKey bool) ([]string, error) {
	request, err := http.NewRequest(http.MethodGet, strings.TrimSpace(modelsURL), nil)
	if err != nil {
		return nil, fmt.Errorf("provider model discovery failed: create request: %w", err)
	}
	request.Header.Set("accept", "application/json")
	if useProviderKey {
		request.Header.Set("authorization", "Bearer "+apiKey)
	}
	response, err := wopalModelsHTTPClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("provider model discovery failed: request models endpoint: %w", err)
	}
	defer response.Body.Close()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxModelMetadataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("provider model discovery failed: read response: %w", err)
	}
	if len(body) > maxModelMetadataBytes {
		return nil, fmt.Errorf("provider model discovery failed: response exceeds %d byte limit", maxModelMetadataBytes)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("provider model discovery failed: HTTP %d%s", response.StatusCode, wopalAPIErrorDetail(body))
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Data == nil {
		return nil, fmt.Errorf("provider model discovery failed: response is missing data[]")
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		if id := strings.TrimSpace(item.ID); id != "" {
			models = append(models, id)
		}
	}
	return models, nil
}
