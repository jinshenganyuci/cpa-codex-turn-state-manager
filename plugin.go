package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	pluginName        = "codex-turn-state-manager"
	pluginVersion     = "0.2.2"
	pluginSchema      = uint32(4)
	pluginABIVersion  = uint32(1)
	defaultMaxBytes   = 4096
	maxStateFileBytes = 16 << 20
	turnStateHeader   = "X-Codex-Turn-State"
)

const (
	methodPluginRegister               = "plugin.register"
	methodPluginQuiesce                = "plugin.quiesce"
	methodPluginReconfigure            = "plugin.reconfigure"
	methodPluginShutdown               = "plugin.shutdown"
	methodRequestInterceptBefore       = "request.intercept_before"
	methodRequestInterceptAfter        = "request.intercept_after"
	methodRequestComplete              = "request.complete"
	methodResponseInterceptAfter       = "response.intercept_after"
	methodResponseInterceptStreamChunk = "response.intercept_stream_chunk"
)

type pluginConfig struct {
	Store             yaml.Node                   `yaml:"store,omitempty"`
	Enabled           bool                        `yaml:"enabled"`
	Priority          int                         `yaml:"priority"`
	AutoUpdate        *bool                       `yaml:"auto_update"`
	Mode              string                      `yaml:"mode"`
	DryRun            bool                        `yaml:"dry_run"`
	BlockWithoutState bool                        `yaml:"block_without_state"`
	RequireModelMatch *bool                       `yaml:"require_model_match"`
	Prefer292         *bool                       `yaml:"prefer_292"`
	StandbyEnabled    *bool                       `yaml:"standby_enabled"`
	RuntimeFile       string                      `yaml:"runtime_file"`
	ArchiveDir        string                      `yaml:"archive_dir"`
	StateFile         string                      `yaml:"state_file"`
	SelectionRequired bool                        `yaml:"selection_required"`
	SelectionFile     string                      `yaml:"selection_file"`
	MaxStateBytes     int                         `yaml:"max_state_bytes"`
	Defaults          *credentialConfig           `yaml:"defaults"`
	Credentials       map[string]credentialConfig `yaml:"credentials"`
	Probe             probeConfig                 `yaml:"probe"`
}

type credentialConfig struct {
	Plan           string   `yaml:"plan"`
	NormalBlocks   int      `yaml:"normal_blocks"`
	AcceptedBlocks []int    `yaml:"accepted_blocks"`
	State          string   `yaml:"state"`
	StateIdentity  string   `yaml:"state_identity"`
	StateModel     string   `yaml:"state_model"`
	Models         []string `yaml:"models"`
	AutoUpdate     *bool    `yaml:"auto_update"`
}

type storedState struct {
	ResponseModel string    `json:"response_model,omitempty"`
	Source        string    `json:"source,omitempty"`
	CapturedAt    time.Time `json:"captured_at,omitempty"`
	Identity      string    `json:"identity"`
	Value         string    `json:"state"`
	IssuedAt      time.Time `json:"issued_at"`
	Blocks        int       `json:"blocks"`
}

type persistedFile struct {
	Version     int                    `json:"version"`
	Credentials map[string]storedState `json:"credentials"`
	Standby     map[string]storedState `json:"standby,omitempty"`
}

type requestBinding struct {
	UsedStateDigest     string
	Display             credentialDisplay
	RequestedModel      string
	ResponseModel       string
	ResponseModelSource string
	AuthID              string
	Key                 string
	StreamBuffer        []byte
	Identity            string
	Failed              bool
	Completed           bool
	Observed            int
	Action              string
	Started             time.Time
	Reasoning           string
}

type stateCandidate struct {
	AuthID string
	Key    string
	State  storedState
}

type runtimeState struct {
	storageDir       string
	journalMu        sync.Mutex
	standby          map[string]storedState
	runtimeLoaded    string
	persistenceError string
	journalRevision  uint64
	journalSaved     uint64
	slotsRevision    uint64
	slotsSaved       uint64
	mu               sync.Mutex
	persistMu        sync.Mutex
	selectionMu      sync.Mutex
	selection        selectionPolicy
	now              func() time.Time
	accepting        bool
	config           pluginConfig
	current          map[string]storedState
	requests         map[string]requestBinding
	candidates       map[string]stateCandidate
	hostCall         func(string, any, any) error
	probeCtx         context.Context
	probeCancel      context.CancelFunc
	generation       uint64
	probing          map[string]bool
	lastProbe        map[string]time.Time
	probeResults     map[string]string
	fetch            func(context.Context, probeAuth, string, *proxyEndpoint, proxyEndpoint) (string, string, string)
	workerDone       chan struct{}
	wake             chan struct{}
	refreshRequests  map[string]string
	probeReasons     map[string]string
	blockedUntil     map[string]time.Time
	history          []observation
	probeCounts      map[string]int
	budgetStart      time.Time
	activeCancels    map[string]context.CancelFunc
	probeWG          sync.WaitGroup
	observed         map[string]int
}

var runtime = newRuntimeState()

func newRuntimeState() *runtimeState {
	return &runtimeState{
		activeCancels:   make(map[string]context.CancelFunc),
		standby:         make(map[string]storedState),
		selection:       emptySelection(),
		now:             time.Now,
		observed:        make(map[string]int),
		probeCounts:     make(map[string]int),
		fetch:           fetchProbe,
		current:         make(map[string]storedState),
		requests:        make(map[string]requestBinding),
		candidates:      make(map[string]stateCandidate),
		probing:         make(map[string]bool),
		lastProbe:       make(map[string]time.Time),
		probeResults:    make(map[string]string),
		refreshRequests: make(map[string]string),
		probeReasons:    make(map[string]string),
		blockedUntil:    make(map[string]time.Time),
	}
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      registrationMetadata   `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationMetadata struct {
	Name             string        `json:"Name"`
	Version          string        `json:"Version"`
	Author           string        `json:"Author"`
	GitHubRepository string        `json:"GitHubRepository"`
	Logo             string        `json:"Logo"`
	ConfigFields     []configField `json:"ConfigFields"`
}

type configField struct {
	Name        string `json:"Name"`
	Type        string `json:"Type"`
	Description string `json:"Description"`
}

type registrationCapability struct {
	ManagementAPI          bool `json:"management_api"`
	RequestInterceptor     bool `json:"request_interceptor"`
	RequestLifecyclePlugin bool `json:"request_lifecycle_plugin"`
	ResponseInterceptor    bool `json:"response_interceptor"`
	StreamChunkInterceptor bool `json:"response_stream_interceptor"`
	WebSocketResponse      bool `json:"websocket_response_observer"`
}

type requestInterceptRequest struct {
	RequestID      string         `json:"RequestID"`
	TraceID        string         `json:"TraceID"`
	SourceFormat   string         `json:"SourceFormat"`
	ToFormat       string         `json:"ToFormat"`
	Model          string         `json:"Model"`
	RequestedModel string         `json:"RequestedModel"`
	Stream         bool           `json:"Stream"`
	Headers        http.Header    `json:"Headers"`
	Body           []byte         `json:"Body"`
	Metadata       map[string]any `json:"Metadata"`
}

type requestInterceptResponse struct {
	Headers         http.Header `json:"Headers,omitempty"`
	ClearHeaders    []string    `json:"ClearHeaders,omitempty"`
	Terminate       bool        `json:"Terminate,omitempty"`
	StatusCode      int         `json:"StatusCode,omitempty"`
	ResponseHeaders http.Header `json:"ResponseHeaders,omitempty"`
	ResponseBody    []byte      `json:"ResponseBody,omitempty"`
}

type responseInterceptRequest struct {
	Body            []byte         `json:"Body"`
	RequestID       string         `json:"RequestID"`
	ResponseHeaders http.Header    `json:"ResponseHeaders"`
	Metadata        map[string]any `json:"Metadata"`
}

type responseInterceptResponse struct{}

type streamChunkInterceptRequest struct {
	RequestID       string         `json:"RequestID"`
	ResponseHeaders http.Header    `json:"ResponseHeaders"`
	ChunkIndex      int            `json:"ChunkIndex"`
	Metadata        map[string]any `json:"Metadata"`
	Body            []byte         `json:"Body"`
}

type streamChunkInterceptResponse struct{}

type requestCompletion struct {
	RequestID  string         `json:"RequestID"`
	Outcome    string         `json:"Outcome"`
	Metadata   map[string]any `json:"Metadata"`
	StatusCode int            `json:"StatusCode"`
	Error      string         `json:"Error"`
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case "management.register":
		return okEnvelope(managementRoutes())
	case "management.handle":
		return runtime.management(rawRequest(request))
	case "websocket.response_event":
		return runtime.observeWebSocket(request)
	case methodPluginRegister, methodPluginReconfigure:
		if errConfigure := runtime.configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case methodPluginQuiesce:
		runtime.setAccepting(false)
		return okEnvelope(struct{}{})
	case methodPluginShutdown:
		runtime.shutdown()
		return okEnvelope(struct{}{})
	case methodRequestInterceptBefore:
		return okEnvelope(requestInterceptResponse{})
	case methodRequestInterceptAfter:
		return runtime.interceptAfter(request)
	case methodResponseInterceptAfter:
		return runtime.interceptResponse(request)
	case methodResponseInterceptStreamChunk:
		return runtime.interceptStreamChunk(request)
	case methodRequestComplete:
		return runtime.complete(request)
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginSchema,
		Metadata: registrationMetadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "jinshenganyuci",
			GitHubRepository: RepositoryURL,
			ConfigFields: []configField{
				{Name: "auto_update", Type: "boolean", Description: "Promote a newer normal Fernet state after a successful request."},
				{Name: "mode", Type: "string", Description: "force (default), observe, or replace_only. Missing cache passes through by default."},
				{Name: "require_model_match", Type: "boolean", Description: "Require matching reported and executed models before admitting state (default true)."},
				{Name: "prefer_292", Type: "boolean", Description: "Prefer 292; use 332 while continuing acquisition (default true)."},
				{Name: "standby_enabled", Type: "boolean", Description: "Keep a successor for expiry handoff (default true)."},
				{Name: "runtime_file", Type: "string", Description: "Private persistent request history and acquisition backoff."},
				{Name: "block_without_state", Type: "boolean", Description: "Optional legacy guard, disabled by default. In force mode, reject selected requests without valid cached state; disabled in dry run."},
				{Name: "archive_dir", Type: "string", Description: "Private archive for successful 292- and 332-character states."},
				{Name: "state_file", Type: "string", Description: "Optional private JSON file used to persist refreshed states."},
				{Name: "selection_required", Type: "boolean", Description: "Require checked credentials and models before acquisition and reuse (default true)."},
				{Name: "selection_file", Type: "string", Description: "Private file persisting dashboard credential and model selections."},
				{Name: "defaults", Type: "object", Description: "Fallback policy for selected auth IDs not listed under credentials."},
				{Name: "credentials", Type: "object", Description: "Per-auth state, plan, model scope, and baseline configuration."},
				{Name: "probe", Type: "object", Description: "Acquire independently per credential/model using only the credential proxy."},
			},
		},
		Capabilities: registrationCapability{
			ManagementAPI:          true,
			RequestInterceptor:     true,
			RequestLifecyclePlugin: true,
			ResponseInterceptor:    true,
			StreamChunkInterceptor: true,
			WebSocketResponse:      true,
		},
	}
}

func (state *runtimeState) configure(raw []byte) error {
	var req lifecycleRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return fmt.Errorf("decode lifecycle request: %w", errUnmarshal)
	}
	if req.SchemaVersion < pluginSchema {
		return fmt.Errorf("%s requires host schema version %d or newer", pluginName, pluginSchema)
	}

	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		decoder := yaml.NewDecoder(bytes.NewReader(req.ConfigYAML))
		decoder.KnownFields(true)
		if errDecode := decoder.Decode(&cfg); errDecode != nil {
			return errors.New("invalid plugin YAML configuration")
		}
	}
	if err := state.applyStorageDefaults(&cfg); err != nil {
		return errors.New("private storage directory unavailable")
	}
	if cfg.Mode != "observe" && cfg.Mode != "replace_only" && cfg.Mode != "force" {
		return errors.New("mode must be observe, replace_only, or force")
	}
	if cfg.MaxStateBytes == 0 {
		cfg.MaxStateBytes = defaultMaxBytes
	}
	if cfg.MaxStateBytes < 256 || cfg.MaxStateBytes > 16384 {
		return errors.New("max_state_bytes must be between 256 and 16384")
	}
	if cfg.Credentials == nil {
		cfg.Credentials = make(map[string]credentialConfig)
	}
	if cfg.RuntimeFile == "" && cfg.StateFile != "" {
		cfg.RuntimeFile = cfg.StateFile + ".runtime.json"
	}
	if cfg.RuntimeFile != "" && (cfg.RuntimeFile == cfg.StateFile || cfg.RuntimeFile == cfg.SelectionFile) {
		return errors.New("runtime_file must differ from state and selection files")
	}
	if cfg.SelectionFile == "" && cfg.StateFile != "" {
		cfg.SelectionFile = cfg.StateFile + ".selection.json"
	}
	if cfg.RuntimeFile != "" && cfg.RuntimeFile == cfg.SelectionFile {
		return errors.New("runtime_file must differ from selection_file")
	}
	if err := normalizeProbe(&cfg.Probe); err != nil {
		return err
	}
	if cfg.Defaults != nil {
		defaults, errNormalize := normalizeCredential(*cfg.Defaults)
		if errNormalize != nil {
			return fmt.Errorf("defaults: %w", errNormalize)
		}
		if strings.TrimSpace(defaults.State) != "" {
			return errors.New("defaults.state is not allowed because state must remain isolated per credential")
		}
		cfg.Defaults = &defaults
	}

	current := make(map[string]storedState)
	for rawAuthID, credential := range cfg.Credentials {
		authID := strings.TrimSpace(rawAuthID)
		if authID == "" || authID != rawAuthID {
			return errors.New("credential IDs must be non-empty and must not have surrounding whitespace")
		}
		var errNormalize error
		credential, errNormalize = normalizeCredential(credential)
		if errNormalize != nil {
			return fmt.Errorf("credential %q: %w", authID, errNormalize)
		}
		cfg.Credentials[authID] = credential
		if strings.TrimSpace(credential.State) == "" {
			continue
		}
		if len(credential.StateIdentity) != 64 {
			return errors.New("seed state requires a 64-character state_identity fingerprint")
		}
		if credential.StateModel == "" || strings.Contains(credential.StateModel, "*") {
			return errors.New("a seed state requires an exact state_model; cross-model seeds are unsafe")
		}
		parsed, errParse := parseTurnState(credential.State, cfg.MaxStateBytes)
		if errParse != nil {
			return fmt.Errorf("credential %q state: %w", authID, errParse)
		}
		if !normalBlockCount(credential, parsed.Blocks) {
			return fmt.Errorf("credential %q state has %d blocks, which does not match its normal baseline", authID, parsed.Blocks)
		}
		current[stateKey(authID, credential.StateModel)] = storedState{Value: strings.TrimSpace(credential.State), IssuedAt: parsed.IssuedAt, Blocks: parsed.Blocks, Identity: credential.StateIdentity}
	}

	persisted, errLoad := loadPersisted(cfg.StateFile, cfg.MaxStateBytes)
	if errLoad != nil {
		return errLoad
	}
	for key, candidate := range persisted {
		authID, model := splitStateKey(key)
		credential, configured := credentialFor(cfg, authID)
		if !configured || model == "" || !matchesModels(credential.Models, model) || !normalBlockCount(credential, candidate.Blocks) {
			continue
		}
		if existing, exists := current[key]; !exists || !candidate.IssuedAt.Before(existing.IssuedAt) {
			current[key] = candidate
		}
	}

	standby, errStandby := loadStandby(cfg.StateFile, cfg.MaxStateBytes)
	if errStandby != nil {
		return errStandby
	}
	journal, errJournal := loadJournal(cfg.RuntimeFile)
	if errJournal != nil {
		return errJournal
	}
	state.selectionMu.Lock()
	defer state.selectionMu.Unlock()
	selection, errSelection := loadSelection(cfg.SelectionFile)
	if errSelection != nil {
		return errSelection
	}
	state.stopBackground()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.probeCancel != nil {
		state.probeCancel()
	}
	state.generation++
	state.probeCtx, state.probeCancel = context.WithCancel(context.Background())
	state.probing = make(map[string]bool)
	state.activeCancels = make(map[string]context.CancelFunc)
	state.refreshRequests = make(map[string]string)
	state.probeReasons = make(map[string]string)
	// Preserve in-memory states on hot reconfiguration even without a state file.
	for key, candidate := range state.current {
		authID, model := splitStateKey(key)
		policy, ok := credentialFor(cfg, authID)
		if ok && model != "" && matchesModels(policy.Models, model) && normalBlockCount(policy, candidate.Blocks) && !candidate.IssuedAt.Before(current[key].IssuedAt) {
			current[key] = candidate
		}
	}
	for key, value := range state.standby {
		if old := standby[key]; old.Value == "" || !value.IssuedAt.Before(old.IssuedAt) {
			standby[key] = value
		}
	}
	state.config = cfg
	state.selection = selection
	state.current = current
	state.standby = standby
	state.restoreJournalLocked(cfg.RuntimeFile, journal)
	for key, value := range state.current {
		if !state.storedSlotValidLocked(key, value) {
			delete(state.current, key)
			state.slotsRevision++
		}
	}
	for key, value := range state.standby {
		if !state.storedSlotValidLocked(key, value) {
			delete(state.standby, key)
			state.slotsRevision++
		}
	}
	state.requests = make(map[string]requestBinding)
	state.candidates = make(map[string]stateCandidate)
	state.accepting = cfg.Enabled
	state.startBackgroundLocked()
	return nil
}

func (state *runtimeState) interceptAfter(raw []byte) ([]byte, error) {
	defer state.flushPersistence()
	var req requestInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode after-auth request: %w", errUnmarshal)
	}
	// Retries can also switch to an unmanaged credential/provider. Never retain
	// a previous attempt's candidate just because this attempt is out of scope.
	state.mu.Lock()
	if previous, exists := state.requests[req.RequestID]; exists {
		state.queueRefreshLocked(previous.Key, "credential_retry")
	}
	delete(state.requests, req.RequestID)
	delete(state.candidates, req.RequestID)
	state.mu.Unlock()
	if !strings.EqualFold(strings.TrimSpace(req.ToFormat), "codex") {
		return okEnvelope(requestInterceptResponse{})
	}
	authID := metadataString(req.Metadata, "selected_auth_id")
	if authID == "" {
		return okEnvelope(requestInterceptResponse{})
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		return okEnvelope(requestInterceptResponse{})
	}
	key := stateKey(authID, model)
	identity, display, identityErr := state.selectedCredential(authID)

	state.mu.Lock()
	defer state.mu.Unlock()
	if !state.accepting {
		return okEnvelope(requestInterceptResponse{})
	}
	credential, exists := credentialFor(state.config, authID)
	if !exists || !matchesModels(credential.Models, req.Model, req.RequestedModel) || !state.pairSelectedLocked(authID, model) {
		return okEnvelope(requestInterceptResponse{})
	}
	if identityErr != nil || !state.identitySelectedLocked(authID, model, identity) {
		if state.blockWithoutStateLocked() {
			return state.blockRequestLocked(req, key, "credential_identity_unavailable")
		}
		return okEnvelope(requestInterceptResponse{})
	}
	if req.RequestID != "" && len(state.requests) < 4096 {
		// A host retry may reuse the request ID with another credential. Discard any
		// candidate from the previous attempt before binding the new selected auth.
		delete(state.candidates, req.RequestID)
		state.requests[req.RequestID] = requestBinding{Display: display, RequestedModel: requestedModel(req), AuthID: authID, Key: key, Identity: identity, Started: state.now(), Action: "observe", Reasoning: requestReasoning(req.Body)}
		state.observed[key] = 0
	}
	state.promoteStandbyLocked(key)
	current, exists := state.current[key]
	if exists && current.Identity != identity {
		delete(state.current, key)
		delete(state.standby, key)
		state.slotsRevision++
		exists = false
	}
	if state.blockWithoutStateLocked() && (!exists || !state.usableSlotLocked(key, current)) {
		return state.blockRequestLocked(req, key, "valid_turn_state_required")
	}
	if !exists || !state.usableSlotLocked(key, current) {
		if state.config.Probe.OnMissing {
			state.queueRefreshLocked(key, "missing_state")
		}
		return okEnvelope(requestInterceptResponse{})
	}
	if current.IssuedAt.After(state.now().Add(5*time.Minute)) || state.config.Mode == "observe" {
		return okEnvelope(requestInterceptResponse{})
	}
	if state.config.Mode == "replace_only" && len(headerValue(req.Headers, turnStateHeader)) != 312 {
		return okEnvelope(requestInterceptResponse{})
	}
	binding := state.requests[req.RequestID]
	if state.config.DryRun {
		binding.Action = "would_replace"
	} else {
		binding.Action = "replaced"
		binding.UsedStateDigest = digest(current.Value)
	}
	if req.RequestID != "" {
		state.requests[req.RequestID] = binding
	}
	if state.config.DryRun {
		return okEnvelope(requestInterceptResponse{})
	}
	return okEnvelope(requestInterceptResponse{Headers: http.Header{turnStateHeader: {current.Value}}})
}

func (state *runtimeState) interceptResponse(raw []byte) ([]byte, error) {
	var req responseInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode response interception: %w", errUnmarshal)
	}
	state.captureCandidate(req.RequestID, req.ResponseHeaders)
	state.observeBody(req.RequestID, req.Body)
	return okEnvelope(responseInterceptResponse{})
}

func (state *runtimeState) interceptStreamChunk(raw []byte) ([]byte, error) {
	var req streamChunkInterceptRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, fmt.Errorf("decode stream interception: %w", errUnmarshal)
	}
	if req.ChunkIndex == -1 {
		state.captureCandidate(req.RequestID, req.ResponseHeaders)
	} else {
		state.observeStreamFailure(req.RequestID, req.Body)
	}
	return okEnvelope(streamChunkInterceptResponse{})
}

func (state *runtimeState) captureCandidate(requestID string, headers http.Header) {
	value := strings.TrimSpace(headerValue(headers, turnStateHeader))
	if requestID == "" || value == "" {
		return
	}

	state.mu.Lock()
	defer state.mu.Unlock()
	binding, exists := state.requests[requestID]
	if !exists || !state.accepting {
		return
	}
	credential, exists := credentialFor(state.config, binding.AuthID)
	_, bindingModel := splitStateKey(binding.Key)
	if !exists || !autoUpdateEnabled(state.config, credential) || !state.identitySelectedLocked(binding.AuthID, bindingModel, binding.Identity) {
		return
	}
	binding.Observed = len(value)
	state.observed[binding.Key] = len(value)
	state.requests[requestID] = binding
	parsed, errParse := parseTurnState(value, state.config.MaxStateBytes)
	if errParse != nil || !normalBlockCount(credential, parsed.Blocks) {
		return
	}
	if parsed.IssuedAt.After(state.now().Add(5*time.Minute)) || !state.now().Before(parsed.IssuedAt.Add(turnStateTTL)) {
		return
	}

	state.candidates[requestID] = stateCandidate{
		AuthID: binding.AuthID,
		Key:    binding.Key,
		State:  storedState{Value: value, IssuedAt: parsed.IssuedAt, Blocks: parsed.Blocks, Identity: binding.Identity},
	}
}

func (state *runtimeState) complete(raw []byte) ([]byte, error) {
	defer state.flushPersistence()
	var completion requestCompletion
	if errUnmarshal := json.Unmarshal(raw, &completion); errUnmarshal != nil {
		return nil, fmt.Errorf("decode request completion: %w", errUnmarshal)
	}

	state.mu.Lock()
	owner := state.requests[completion.RequestID]
	state.mu.Unlock()
	currentIdentity, identityErr := state.selectedIdentity(owner.AuthID)

	state.mu.Lock()
	candidate, hasCandidate := state.candidates[completion.RequestID]
	binding := state.requests[completion.RequestID]
	successful := completion.Outcome == "succeeded" && !binding.Failed && binding.Completed
	if identityErr != nil || currentIdentity != binding.Identity {
		hasCandidate = false
	}
	_, bindingModel := splitStateKey(binding.Key)
	if !state.identitySelectedLocked(binding.AuthID, bindingModel, binding.Identity) {
		hasCandidate = false
	}
	rejection := state.modelRejectionLocked(bindingModel, binding.ResponseModel)
	if rejection != "" {
		hasCandidate = false
	}
	if rejection == "model_mismatch" && currentIdentity == binding.Identity {
		state.invalidateUsedStateLocked(binding)
		if !state.usableSlotLocked(binding.Key, state.current[binding.Key]) {
			state.queueRefreshLocked(binding.Key, "model_mismatch")
		}
	}
	cacheAction := ""
	if hasCandidate && state.accepting && successful {
		candidate.State.ResponseModel = binding.ResponseModel
		candidate.State.Source = "business"
		candidate.State.CapturedAt = state.now()
		cacheAction = state.acceptStateLocked(candidate.Key, candidate.State)
		delete(state.refreshRequests, candidate.Key)
	}
	if binding.AuthID != "" {
		state.recordLocked(binding.Key, binding.Observed, successful, binding.Action, binding.Reasoning)
		entry := &state.history[len(state.history)-1]
		entry.credentialDisplay = binding.Display
		entry.RequestedModel = binding.RequestedModel
		entry.ResponseModel = binding.ResponseModel
		entry.ResponseModelSource = binding.ResponseModelSource
		entry.HostOutcome = completion.Outcome
		entry.TerminalCompleted = binding.Completed
		entry.TerminalFailed = binding.Failed
		entry.CacheAction = cacheAction
		entry.StateSource = "business"
		entry.Acceptance = rejection
		if hasCandidate && successful {
			entry.Acceptance = "accepted"
		}
	}
	if binding.AuthID != "" && completion.Outcome == "failed" {
		if reason := failureRefreshReason(completion.StatusCode, completion.Error); reason != "" {
			state.queueRefreshLocked(binding.Key, reason)
		}
	}
	delete(state.candidates, completion.RequestID)
	delete(state.requests, completion.RequestID)
	promoted := acceptedSlotAction(cacheAction)
	archiveDir := state.config.ArchiveDir
	stateFile := state.config.StateFile
	state.mu.Unlock()

	if hasCandidate && successful {
		if err := archiveState(archiveDir, candidate.Key, candidate.State, binding.Reasoning); err != nil {
			return nil, err
		}
	}
	if promoted && strings.TrimSpace(stateFile) != "" {
		if errPersist := state.persistCurrent(stateFile); errPersist != nil {
			return nil, errPersist
		}
	}
	return okEnvelope(struct{}{})
}

func (state *runtimeState) setAccepting(accepting bool) {
	state.mu.Lock()
	state.accepting = accepting
	if !accepting && state.probeCancel != nil {
		state.probeCancel()
	}
	state.mu.Unlock()
	if !accepting {
		state.stopBackground()
		state.flushPersistence()
	}
}

func (state *runtimeState) shutdown() {
	state.setAccepting(false)
	_ = state.persistRuntime()
	if state.config.StateFile != "" {
		_ = state.persistCurrent(state.config.StateFile)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.accepting = false
	if state.probeCancel != nil {
		state.probeCancel()
	}
	state.generation++
	state.requests = make(map[string]requestBinding)
	state.candidates = make(map[string]stateCandidate)
}

func autoUpdateEnabled(cfg pluginConfig, credential credentialConfig) bool {
	if credential.AutoUpdate != nil {
		return *credential.AutoUpdate
	}
	if cfg.AutoUpdate != nil {
		return *cfg.AutoUpdate
	}
	return true
}

func credentialFor(cfg pluginConfig, authID string) (credentialConfig, bool) {
	if credential, exists := cfg.Credentials[authID]; exists {
		return credential, true
	}
	if cfg.Defaults != nil {
		return *cfg.Defaults, true
	}
	return credentialConfig{}, false
}

func normalizeCredential(credential credentialConfig) (credentialConfig, error) {
	credential.Plan = strings.ToLower(strings.TrimSpace(credential.Plan))
	credential.Models = canonicalModels(credential.Models)
	credential.StateModel = strings.TrimSpace(credential.StateModel)
	if credential.NormalBlocks < 0 {
		return credentialConfig{}, errors.New("normal_blocks must not be negative")
	}
	// Both target lengths are usable by default, independent of account plan.
	// Explicit block policies remain authoritative for existing configurations.
	if len(credential.AcceptedBlocks) == 0 && credential.NormalBlocks == 0 {
		credential.AcceptedBlocks = []int{10, 12}
	}
	seen := make(map[int]struct{}, len(credential.AcceptedBlocks))
	accepted := make([]int, 0, len(credential.AcceptedBlocks))
	for _, blocks := range credential.AcceptedBlocks {
		if blocks <= 0 || blocks > 1024 {
			return credentialConfig{}, errors.New("accepted_blocks entries must be between 1 and 1024")
		}
		if _, exists := seen[blocks]; exists {
			continue
		}
		seen[blocks] = struct{}{}
		accepted = append(accepted, blocks)
	}
	sort.Ints(accepted)
	credential.AcceptedBlocks = accepted
	return credential, nil
}

func normalBlockCount(credential credentialConfig, blocks int) bool {
	if len(credential.AcceptedBlocks) > 0 {
		for _, accepted := range credential.AcceptedBlocks {
			if blocks == accepted {
				return true
			}
		}
		return false
	}
	want := credential.NormalBlocks
	if want == 0 {
		switch strings.ToLower(strings.TrimSpace(credential.Plan)) {
		case "pro", "plus":
			want = 10
		case "team":
			want = 12
		default:
			return false
		}
	}
	return blocks == want
}

func canonicalModels(models []string) []string {
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	sort.Strings(out)
	return out
}

func matchesModels(patterns []string, models ...string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, rawModel := range models {
		model := strings.ToLower(strings.TrimSpace(rawModel))
		if model == "" {
			continue
		}
		for _, pattern := range patterns {
			if strings.HasSuffix(pattern, "*") {
				if strings.HasPrefix(model, strings.TrimSuffix(pattern, "*")) {
					return true
				}
				continue
			}
			if model == pattern {
				return true
			}
		}
	}
	return false
}

func metadataString(metadata map[string]any, key string) string {
	value, _ := metadata[key].(string)
	return strings.TrimSpace(value)
}

func loadPersisted(path string, maxBytes int) (map[string]storedState, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, nil
	}
	fileHandle, errOpen := os.Open(path)
	if errors.Is(errOpen, os.ErrNotExist) {
		return nil, nil
	}
	if errOpen != nil {
		return nil, fmt.Errorf("open state file: %w", errOpen)
	}
	defer func() { _ = fileHandle.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(fileHandle, maxStateFileBytes+1))
	if errors.Is(errRead, os.ErrNotExist) {
		return nil, nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("read state file: %w", errRead)
	}
	if len(raw) > maxStateFileBytes {
		return nil, fmt.Errorf("state file exceeds %d bytes", maxStateFileBytes)
	}
	var file persistedFile
	if errUnmarshal := json.Unmarshal(raw, &file); errUnmarshal != nil {
		return nil, fmt.Errorf("decode state file: %w", errUnmarshal)
	}
	// Legacy states have no model identity and cannot safely be reused.
	if file.Version == 1 {
		return nil, nil
	}
	if file.Version != 2 {
		return nil, fmt.Errorf("unsupported state file version %d", file.Version)
	}
	out := make(map[string]storedState, len(file.Credentials))
	for authID, stored := range file.Credentials {
		if authID == "" || authID != strings.TrimSpace(authID) {
			continue
		}
		parsed, errParse := parseTurnState(stored.Value, maxBytes)
		if errParse != nil {
			continue
		}
		stored.IssuedAt = parsed.IssuedAt
		stored.Blocks = parsed.Blocks
		out[authID] = stored
	}
	return out, nil
}

func (state *runtimeState) persistCurrent(path string) error {
	state.persistMu.Lock()
	defer state.persistMu.Unlock()
	state.mu.Lock()
	if path != state.config.StateFile {
		state.mu.Unlock()
		return nil
	}
	snapshot := persistedFile{Version: 2, Credentials: cloneStoredStates(state.current), Standby: cloneStoredStates(state.standby)}
	revision := state.slotsRevision
	state.mu.Unlock()
	err := writePrivateJSON(path, snapshot)
	state.mu.Lock()
	if err == nil {
		state.slotsSaved = revision
	}
	state.mu.Unlock()
	return err
}

func persistStates(path string, states map[string]storedState) error {
	return writePrivateJSON(path, persistedFile{Version: 2, Credentials: states})
}

func writePrivateJSON(path string, value any) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || path == "" {
		return errors.New("state_file path is empty")
	}
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		return fmt.Errorf("create state directory: %w", errMkdir)
	}
	raw, errMarshal := json.MarshalIndent(value, "", "  ")
	if errMarshal != nil {
		return fmt.Errorf("encode state file: %w", errMarshal)
	}
	raw = append(raw, '\n')
	tempFile, errCreate := os.CreateTemp(filepath.Dir(path), ".codex-turn-state-*.tmp")
	if errCreate != nil {
		return fmt.Errorf("create temporary state file: %w", errCreate)
	}
	tempPath := tempFile.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tempPath)
		}
	}()
	if errChmod := tempFile.Chmod(0o600); errChmod != nil {
		_ = tempFile.Close()
		return fmt.Errorf("secure temporary state file: %w", errChmod)
	}
	if _, errWrite := tempFile.Write(raw); errWrite != nil {
		_ = tempFile.Close()
		return fmt.Errorf("write temporary state file: %w", errWrite)
	}
	if errSync := tempFile.Sync(); errSync != nil {
		_ = tempFile.Close()
		return fmt.Errorf("sync temporary state file: %w", errSync)
	}
	if errClose := tempFile.Close(); errClose != nil {
		return fmt.Errorf("close temporary state file: %w", errClose)
	}
	if errRename := os.Rename(tempPath, path); errRename != nil {
		return fmt.Errorf("replace state file: %w", errRename)
	}
	removeTemp = false
	return nil
}

func cloneStoredStates(source map[string]storedState) map[string]storedState {
	out := make(map[string]storedState, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}

func stateKey(authID, model string) string {
	raw, _ := json.Marshal([2]string{authID, strings.TrimSpace(model)})
	return string(raw)
}

func splitStateKey(key string) (string, string) {
	var pair [2]string
	if json.Unmarshal([]byte(key), &pair) != nil {
		return "", ""
	}
	return pair[0], pair[1]
}

func okEnvelope(value any) ([]byte, error) {
	raw, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, errMarshal := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	if errMarshal != nil {
		return []byte(`{"ok":false,"error":{"code":"plugin_error","message":"encode error"}}`)
	}
	return raw
}
