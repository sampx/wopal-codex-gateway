package codex

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// RestoreValue is one managed key's pre-install state.
type RestoreValue struct {
	HadValue bool   `json:"hadValue"`
	Value    string `json:"value,omitempty"`
}

// CodexRestoreState records what `codex install` changed in config.toml.
type CodexRestoreState struct {
	Model                    RestoreValue  `json:"model"`
	ModelCatalogJSON         RestoreValue  `json:"modelCatalogJson"`
	OpenAIBaseURL            RestoreValue  `json:"openaiBaseUrl"`
	ExposedExternalModels    []string      `json:"exposedExternalModels,omitempty"`
	MemoriesSectionExisted   *bool         `json:"memoriesSectionExisted,omitempty"`
	MemoryExtractModel       *RestoreValue `json:"memoryExtractModel,omitempty"`
	MemoryConsolidationModel *RestoreValue `json:"memoryConsolidationModel,omitempty"`
}

const (
	codexManagedModelKey               = "model"
	codexManagedModelCatalogJSONKey    = "model_catalog_json"
	codexManagedOpenAIBaseURLKey       = "openai_base_url"
	codexManagedMemoriesSection        = "memories"
	codexManagedMemoryExtractModelKey  = "extract_model"
	codexManagedMemoryConsolidationKey = "consolidation_model"
	codexManagedMemoryModel            = "wopal-ai/deepseek-v4.1-flash"
)

var (
	blankTomlLineRegex = regexp.MustCompile(`^\s*$`)
	tomlSectionRegex   = regexp.MustCompile(`^\s*\[`)
)

type tomlAssignment struct {
	index  int
	groups []string
}

type tomlSectionRange struct {
	header int
	start  int
	end    int
}

// splitLines keeps each line's trailing newline and omits the synthetic empty
// line produced by a final newline.
func splitLines(text string) []string {
	var lines []string
	for len(text) > 0 {
		if index := strings.IndexByte(text, '\n'); index >= 0 {
			lines = append(lines, text[:index+1])
			text = text[index+1:]
			continue
		}
		lines = append(lines, text)
		break
	}
	return lines
}

func rootEndIndex(lines []string) int {
	for index, line := range lines {
		if tomlSectionRegex.MatchString(line) {
			return index
		}
	}
	return len(lines)
}

func assignmentRegex(key string) *regexp.Regexp {
	return regexp.MustCompile(`^(\s*` + regexp.QuoteMeta(key) + `\s*=\s*)("(?:\\.|[^"\\])*")(\s*(?:#.*)?)(\n?)$`)
}

func quoteTomlString(value string) string {
	quoted, err := json.Marshal(value)
	if err != nil {
		// Strings always marshal; keep the signature faithful to JSON.stringify.
		panic(err)
	}
	return string(quoted)
}

func parseTomlString(raw string) string {
	var value string
	if err := json.Unmarshal([]byte(raw), &value); err == nil {
		return value
	}
	if value, err := strconv.Unquote(raw); err == nil {
		return value
	}
	return strings.Trim(raw, `"`)
}

func findRootAssignment(lines []string, key string) (tomlAssignment, bool) {
	end := rootEndIndex(lines)
	regex := assignmentRegex(key)
	for index := 0; index < end; index++ {
		groups := regex.FindStringSubmatch(lines[index])
		if groups != nil {
			return tomlAssignment{index: index, groups: groups}, true
		}
	}
	return tomlAssignment{}, false
}

func sectionRange(lines []string, section string) (tomlSectionRange, bool) {
	headerRegex := regexp.MustCompile(`^\s*\[` + regexp.QuoteMeta(section) + `\]\s*(?:#.*)?(?:\n)?$`)
	header := -1
	for index, line := range lines {
		if headerRegex.MatchString(line) {
			header = index
			break
		}
	}
	if header < 0 {
		return tomlSectionRange{}, false
	}
	end := header + 1
	for end < len(lines) && !tomlSectionRegex.MatchString(lines[end]) {
		end++
	}
	return tomlSectionRange{header: header, start: header + 1, end: end}, true
}

func findSectionAssignment(lines []string, section, key string) (tomlAssignment, bool) {
	rng, ok := sectionRange(lines, section)
	if !ok {
		return tomlAssignment{}, false
	}
	regex := assignmentRegex(key)
	for index := rng.start; index < rng.end; index++ {
		groups := regex.FindStringSubmatch(lines[index])
		if groups != nil {
			return tomlAssignment{index: index, groups: groups}, true
		}
	}
	return tomlAssignment{}, false
}

func getRootString(text, key string) RestoreValue {
	found, ok := findRootAssignment(splitLines(text), key)
	if !ok {
		return RestoreValue{}
	}
	return RestoreValue{HadValue: true, Value: parseTomlString(found.groups[2])}
}

func getSectionString(text, section, key string) RestoreValue {
	found, ok := findSectionAssignment(splitLines(text), section, key)
	if !ok {
		return RestoreValue{}
	}
	return RestoreValue{HadValue: true, Value: parseTomlString(found.groups[2])}
}

func setRootString(text, key, value string) string {
	lines := splitLines(text)
	if found, ok := findRootAssignment(lines, key); ok {
		lines[found.index] = found.groups[1] + quoteTomlString(value) + found.groups[3] + found.groups[4]
		return strings.Join(lines, "")
	}

	insertion := rootEndIndex(lines)
	for insertion > 0 && blankTomlLineRegex.MatchString(lines[insertion-1]) {
		insertion--
	}
	newline := ""
	if text != "" && !strings.HasSuffix(text, "\n") {
		newline = "\n"
	}
	lines = append(lines, "")
	copy(lines[insertion+1:], lines[insertion:])
	lines[insertion] = newline + key + " = " + quoteTomlString(value) + "\n"
	return strings.Join(lines, "")
}

func setSectionString(text, section, key, value string) string {
	lines := splitLines(text)
	if found, ok := findSectionAssignment(lines, section, key); ok {
		lines[found.index] = found.groups[1] + quoteTomlString(value) + found.groups[3] + found.groups[4]
		return strings.Join(lines, "")
	}

	if rng, ok := sectionRange(lines, section); ok {
		insertion := rng.end
		for insertion > rng.start && blankTomlLineRegex.MatchString(lines[insertion-1]) {
			insertion--
		}
		lines = append(lines, "")
		copy(lines[insertion+1:], lines[insertion:])
		lines[insertion] = key + " = " + quoteTomlString(value) + "\n"
		return strings.Join(lines, "")
	}

	separator := "\n\n"
	if text == "" || strings.HasSuffix(text, "\n\n") {
		separator = ""
	} else if strings.HasSuffix(text, "\n") {
		separator = "\n"
	}
	return text + separator + "[" + section + "]\n" + key + " = " + quoteTomlString(value) + "\n"
}

func removeRootValue(text, key string) string {
	lines := splitLines(text)
	found, ok := findRootAssignment(lines, key)
	if !ok {
		return text
	}
	lines = append(lines[:found.index], lines[found.index+1:]...)
	return strings.Join(lines, "")
}

func removeSectionValue(text, section, key string) string {
	lines := splitLines(text)
	found, ok := findSectionAssignment(lines, section, key)
	if !ok {
		return text
	}
	lines = append(lines[:found.index], lines[found.index+1:]...)
	return strings.Join(lines, "")
}

func removeSectionIfEmpty(text, section string) string {
	lines := splitLines(text)
	rng, ok := sectionRange(lines, section)
	if !ok {
		return text
	}
	for _, line := range lines[rng.start:rng.end] {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			return text
		}
	}
	start := rng.header
	if start > 0 && blankTomlLineRegex.MatchString(lines[start-1]) {
		start--
	}
	lines = append(lines[:start], lines[rng.end:]...)
	return strings.Join(lines, "")
}

func captureRestoreState(text string) CodexRestoreState {
	lines := splitLines(text)
	_, memoriesSectionExisted := sectionRange(lines, codexManagedMemoriesSection)
	extractModel := getSectionString(text, codexManagedMemoriesSection, codexManagedMemoryExtractModelKey)
	consolidationModel := getSectionString(text, codexManagedMemoriesSection, codexManagedMemoryConsolidationKey)
	return CodexRestoreState{
		Model:                    getRootString(text, codexManagedModelKey),
		ModelCatalogJSON:         getRootString(text, codexManagedModelCatalogJSONKey),
		OpenAIBaseURL:            getRootString(text, codexManagedOpenAIBaseURLKey),
		MemoriesSectionExisted:   &memoriesSectionExisted,
		MemoryExtractModel:       &extractModel,
		MemoryConsolidationModel: &consolidationModel,
	}
}

func mergeGatewayRestoreState(saved, current CodexRestoreState) CodexRestoreState {
	merged := saved
	merged.ExposedExternalModels = mergeExposedExternalModels(saved.ExposedExternalModels, current.ExposedExternalModels)
	if merged.MemoriesSectionExisted == nil {
		merged.MemoriesSectionExisted = current.MemoriesSectionExisted
	}
	if merged.MemoryExtractModel == nil {
		merged.MemoryExtractModel = current.MemoryExtractModel
	}
	if merged.MemoryConsolidationModel == nil {
		merged.MemoryConsolidationModel = current.MemoryConsolidationModel
	}
	return merged
}

func mergeExposedExternalModels(existing []string, groups ...[]string) []string {
	seen := make(map[string]struct{}, len(existing))
	result := make([]string, 0, len(existing))
	appendGroup := func(values []string) {
		for _, value := range values {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	appendGroup(existing)
	for _, group := range groups {
		appendGroup(group)
	}
	return result
}

func restoreStateOwnsModel(state CodexRestoreState, model string) bool {
	model = strings.TrimSpace(model)
	if len(state.ExposedExternalModels) == 0 {
		// Compatibility with restore state written before exposed names were
		// recorded explicitly.
		return strings.HasPrefix(model, "wopal-ai/")
	}
	for _, exposed := range state.ExposedExternalModels {
		if strings.TrimSpace(exposed) == model {
			return true
		}
	}
	return false
}

func applyGatewayConfig(text, modelCatalogPath, gatewayBaseURL string) string {
	return applyGatewayConfigWithMemoryModel(text, modelCatalogPath, gatewayBaseURL, codexManagedMemoryModel)
}

func applyGatewayConfigWithMemoryModel(text, modelCatalogPath, gatewayBaseURL, memoryModel string) string {
	next := setRootString(text, codexManagedModelCatalogJSONKey, modelCatalogPath)
	next = setRootString(next, codexManagedOpenAIBaseURLKey, gatewayBaseURL)
	next = setSectionString(next, codexManagedMemoriesSection, codexManagedMemoryExtractModelKey, memoryModel)
	next = setSectionString(next, codexManagedMemoriesSection, codexManagedMemoryConsolidationKey, memoryModel)
	return next
}

func restoreValue(text, key string, state RestoreValue) string {
	if state.HadValue {
		return setRootString(text, key, state.Value)
	}
	return removeRootValue(text, key)
}

func restoreSectionValue(text, section, key string, state RestoreValue) string {
	if state.HadValue {
		return setSectionString(text, section, key, state.Value)
	}
	return removeSectionValue(text, section, key)
}

func restoreGatewayConfig(text string, state CodexRestoreState) string {
	next := text
	currentModel := getRootString(text, codexManagedModelKey)
	if currentModel.HadValue && restoreStateOwnsModel(state, currentModel.Value) {
		next = restoreValue(next, codexManagedModelKey, state.Model)
	}
	next = restoreValue(next, codexManagedModelCatalogJSONKey, state.ModelCatalogJSON)
	next = restoreValue(next, codexManagedOpenAIBaseURLKey, state.OpenAIBaseURL)
	if state.MemoryExtractModel != nil {
		next = restoreSectionValue(next, codexManagedMemoriesSection, codexManagedMemoryExtractModelKey, *state.MemoryExtractModel)
	}
	if state.MemoryConsolidationModel != nil {
		next = restoreSectionValue(next, codexManagedMemoriesSection, codexManagedMemoryConsolidationKey, *state.MemoryConsolidationModel)
	}
	if state.MemoriesSectionExisted != nil && !*state.MemoriesSectionExisted {
		next = removeSectionIfEmpty(next, codexManagedMemoriesSection)
	}
	return next
}
