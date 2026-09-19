package main

import (
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"unicode"
)

// Display metadata never participates in credential identity or cache keys.
type credentialDisplay struct {
	Email    string `json:"email,omitempty"`
	PlanType string `json:"plan_type,omitempty"`
	Label    string `json:"credential_label,omitempty"`
}

type credentialMetadata struct {
	AccountID       string `json:"account_id"`
	Disabled        bool   `json:"disabled"`
	Email           string `json:"email"`
	PlanType        string `json:"plan_type"`
	ChatGPTPlanType string `json:"chatgpt_plan_type"`
	IDToken         string `json:"id_token"`
	AccessToken     string `json:"access_token"`
}

func displayText(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

func planType(value string) string {
	switch value = strings.ToLower(strings.TrimSpace(value)); value {
	case "free", "plus", "pro", "team", "business", "enterprise", "edu", "go":
		return value
	default:
		return ""
	}
}

func credentialPresentation(authID, fallbackEmail string, metadata credentialMetadata) credentialDisplay {
	display := credentialDisplay{Label: filepath.Base(authID), Email: displayText(metadata.Email, 320), PlanType: planType(metadata.PlanType)}
	if display.Email == "" {
		display.Email = displayText(fallbackEmail, 320)
	}
	if display.PlanType == "" {
		display.PlanType = planType(metadata.ChatGPTPlanType)
	}
	// Claims are only a display hint from the host credential, not signature or subscription verification.
	for _, token := range []string{metadata.IDToken, metadata.AccessToken} {
		parts := strings.Split(token, ".")
		if len(parts) != 3 || len(parts[1]) > 64<<10 {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		if err != nil {
			continue
		}
		var claims struct {
			Email string `json:"email"`
			Auth  struct {
				Plan string `json:"chatgpt_plan_type"`
			} `json:"https://api.openai.com/auth"`
			Profile struct {
				Email string `json:"email"`
			} `json:"https://api.openai.com/profile"`
		}
		if json.Unmarshal(raw, &claims) != nil {
			continue
		}
		if display.PlanType == "" {
			display.PlanType = planType(claims.Auth.Plan)
		}
		if display.Email == "" {
			display.Email = displayText(claims.Email, 320)
		}
		if display.Email == "" {
			display.Email = displayText(claims.Profile.Email, 320)
		}
	}
	return display
}

func (state *runtimeState) listDisplayAuths() (authListing, error) {
	listing, err := state.listAuths()
	if err != nil {
		return listing, err
	}
	for i := range listing.Files {
		entry := &listing.Files[i]
		if entry.Provider != "codex" {
			continue
		}
		var result struct {
			JSON credentialMetadata `json:"json"`
		}
		if entry.Index != "" {
			_ = state.hostCall("host.auth.get", map[string]string{"auth_index": entry.Index}, &result)
		}
		display := credentialPresentation(entry.ID, entry.Email, result.JSON)
		entry.Email, entry.PlanType = display.Email, display.PlanType
	}
	return listing, nil
}

func requestedModel(req requestInterceptRequest) string {
	if model := displayText(req.RequestedModel, 256); model != "" {
		return model
	}
	return displayText(req.Model, 256)
}

// Terminal Responses models are retained by CPA. Initial events and translated
// Chat Completions chunks can contain a request-model fallback, so do not infer from them.
func observeModel(binding *requestBinding, raw []byte, upstream bool) {
	var event struct {
		Type     string `json:"type"`
		Object   string `json:"object"`
		Model    string `json:"model"`
		Status   string `json:"status"`
		Response struct {
			Model string `json:"model"`
		} `json:"response"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return
	}
	model, source := "", ""
	if event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.failed" || upstream && (event.Type == "response.created" || event.Type == "response.in_progress") {
		model, source = event.Response.Model, event.Type+".response.model"
	} else if event.Type == "" && (event.Object == "response" || event.Object == "chat.completion" || event.Status == "completed" || event.Status == "incomplete") {
		model, source = event.Model, "response.model"
	}
	model = displayText(model, 256)
	if model == "" || model == "model" {
		return
	}
	if upstream {
		source = "upstream." + source
	}
	if !upstream && strings.HasPrefix(binding.ResponseModelSource, "upstream.") {
		return
	}
	binding.ResponseModel, binding.ResponseModelSource = model, source
}
