package laya

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/domain"
)

type request struct {
	SchemaVersion string           `json:"schema_version"`
	State         classifier.Input `json:"state"`
}

type response struct {
	SchemaVersion string `json:"schema_version"`
	Classifier    struct {
		Name       string `json:"name"`
		Repository string `json:"repository"`
		Revision   string `json:"revision"`
		Variant    string `json:"variant"`
	} `json:"classifier"`
	Answers map[string]answer `json:"answers"`
}

type answer struct {
	Type          string              `json:"type"`
	Choice        string              `json:"choice"`
	Probabilities map[string]*float64 `json:"probabilities"`
	Confidence    *float64            `json:"confidence"`
	Score         *float64            `json:"score"`
	Legend        map[string]string   `json:"legend"`
	Noul          *float64            `json:"noul"`
}

func parseJudgment(body []byte) (domain.ClassifierJudgment, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var wire response
	if decoder.Decode(&wire) != nil || decoder.Decode(&struct{}{}) != io.EOF || wire.SchemaVersion != schemaVersion || wire.Classifier.Name != "laya" || wire.Classifier.Repository != "convaiinnovations/laya" || strings.TrimSpace(wire.Classifier.Revision) == "" || wire.Classifier.Variant != "typed-decisions" {
		return domain.ClassifierJudgment{}, classifier.ErrUnavailable
	}
	tier, task := wire.Answers["minimum_tier"], wire.Answers["task_type"]
	reasoning, coding, risk := wire.Answers["reasoning_required"], wire.Answers["coding_required"], wire.Answers["blast_radius"]
	noul := wire.Answers["underspecified"]
	if len(wire.Answers) != 6 || !validChoice(tier, tierKeys()) || !validChoice(task, taskKeys()) || !validScore(reasoning, 6) || !validScore(coding, 6) || !validScore(risk, 4) || noul.Type != "noul" || !inRange(noul.Noul, 1) {
		return domain.ClassifierJudgment{}, classifier.ErrUnavailable
	}
	minimum, _ := domain.ParseTier(tier.Choice)
	judgment := domain.ClassifierJudgment{MinimumTier: minimum, TierConfidence: *tier.Confidence, TierProbabilities: make(map[domain.Tier]float64, len(tier.Probabilities)), TaskType: domain.TaskType(task.Choice), TaskTypeConfidence: *task.Confidence, TaskTypeProbabilities: make(map[domain.TaskType]float64, len(task.Probabilities)), ReasoningScore: *reasoning.Score, ReasoningConfidence: *reasoning.Confidence, CodingScore: *coding.Score, CodingConfidence: *coding.Confidence, BlastRadius: *risk.Score, BlastRadiusConfidence: *risk.Confidence, Underspecified: *noul.Noul, Classifier: "laya", ResolvedModel: wire.Classifier.Repository + "/" + wire.Classifier.Variant, ModelRevision: wire.Classifier.Revision}
	for key, value := range tier.Probabilities {
		parsed, _ := domain.ParseTier(key)
		judgment.TierProbabilities[parsed] = *value
	}
	for key, value := range task.Probabilities {
		judgment.TaskTypeProbabilities[domain.TaskType(key)] = *value
	}
	return judgment, nil
}

func tierKeys() map[string]bool {
	keys := make(map[string]bool, 7)
	for tier := domain.T0; tier <= domain.T6; tier++ {
		keys[tier.String()] = true
	}
	return keys
}

func taskKeys() map[string]bool {
	return map[string]bool{"unknown": true, "extraction": true, "classification": true, "generation": true, "reasoning": true, "coding": true, "tool_use": true, "multimodal": true}
}

func validChoice(value answer, keys map[string]bool) bool {
	return value.Type == "choice" && keys[value.Choice] && inRange(value.Confidence, 1) && value.Probabilities[value.Choice] != nil && validDistribution(value.Probabilities, keys)
}

func validScore(value answer, maximum float64) bool {
	keys := make(map[string]bool, int(maximum)+1)
	for index := 0; float64(index) <= maximum; index++ {
		key := strconv.Itoa(index)
		keys[key] = true
		if strings.TrimSpace(value.Legend[key]) == "" {
			return false
		}
	}
	return value.Type == "score" && inRange(value.Score, maximum) && inRange(value.Confidence, 1) && len(value.Legend) == len(keys) && validDistribution(value.Probabilities, keys)
}

func validDistribution(values map[string]*float64, keys map[string]bool) bool {
	if len(values) != len(keys) {
		return false
	}
	sum := 0.0
	for key, value := range values {
		if !keys[key] || !inRange(value, 1) {
			return false
		}
		sum += *value
	}
	return math.Abs(sum-1) <= 1e-3
}

func inRange(value *float64, maximum float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0) && *value >= 0 && *value <= maximum
}
