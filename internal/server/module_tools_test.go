package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jhlee-young/cosmos-mcp/internal/cosmos"
)

func TestGetProposalTallyFallsBackFromV1ToV1beta1(t *testing.T) {
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/gov/v1/proposals/42/tally":
			http.NotFound(w, r)
		case "/cosmos/gov/v1beta1/proposals/42/tally":
			_ = json.NewEncoder(w).Encode(map[string]any{"tally": map[string]any{"yes": "10", "no": "2"}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	_, toolResp, err := s.getProposalTally(context.Background(), nil, proposalInput{ProposalID: "42"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getProposalTally() error = %v, toolError = %#v", err, toolResp.Error)
	}
	if toolResp.Meta.Binding != "/cosmos/gov/v1beta1/proposals/42/tally" {
		t.Fatalf("binding = %s", toolResp.Meta.Binding)
	}
	tally, _ := toolResp.Data.(map[string]any)["tally"].(map[string]any)
	if tally["yes"] != "10" {
		t.Fatalf("tally = %#v", tally)
	}
}

func TestGetProposalTallyRejectsInvalidID(t *testing.T) {
	s := &Server{query: cosmos.NewResolver(nil, nil, nil)}
	for _, id := range []string{"", "abc", "0", "-1"} {
		_, toolResp, err := s.getProposalTally(context.Background(), nil, proposalInput{ProposalID: id})
		if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeInvalidInput {
			t.Fatalf("getProposalTally(%q) = %#v, err=%v", id, toolResp.Error, err)
		}
	}
}

func TestGetProposalVotesListAndSingleVoter(t *testing.T) {
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/gov/v1/proposals/7/votes":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"votes":      []any{map[string]any{"voter": "cosmos1abc"}},
				"pagination": map[string]any{"next_key": "more", "total": "1"},
			})
		case "/cosmos/gov/v1/proposals/7/votes/cosmos1abc":
			_ = json.NewEncoder(w).Encode(map[string]any{"vote": map[string]any{"voter": "cosmos1abc"}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	_, toolResp, err := s.getProposalVotes(context.Background(), nil, proposalVotesInput{ProposalID: "7"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getProposalVotes() list error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	if votes, _ := data["votes"].([]any); len(votes) != 1 {
		t.Fatalf("votes = %#v", data["votes"])
	}
	if page, _ := data["pagination"].(map[string]any); page["next_key"] != "more" {
		t.Fatalf("list query must surface the continuation key, got %#v", data["pagination"])
	}

	_, toolResp, err = s.getProposalVotes(context.Background(), nil, proposalVotesInput{ProposalID: "7", Voter: "cosmos1abc"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getProposalVotes() single error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ = toolResp.Data.(map[string]any)
	vote, _ := data["vote"].(map[string]any)
	if vote["voter"] != "cosmos1abc" {
		t.Fatalf("vote = %#v", data["vote"])
	}
}

func TestGetSigningInfosListAndSingleAddress(t *testing.T) {
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/slashing/v1beta1/signing_infos":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"info":       []any{map[string]any{"address": "cosmosvalcons1abc", "jailed_until": "1970-01-01T00:00:00Z"}},
				"pagination": map[string]any{"next_key": nil, "total": "1"},
			})
		case "/cosmos/slashing/v1beta1/signing_infos/cosmosvalcons1abc":
			_ = json.NewEncoder(w).Encode(map[string]any{"val_signing_info": map[string]any{"address": "cosmosvalcons1abc"}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	_, toolResp, err := s.getSigningInfos(context.Background(), nil, signingInfosInput{})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getSigningInfos() list error = %v, toolError = %#v", err, toolResp.Error)
	}
	if info, _ := toolResp.Data.(map[string]any)["info"].([]any); len(info) != 1 {
		t.Fatalf("info = %#v", toolResp.Data)
	}

	_, toolResp, err = s.getSigningInfos(context.Background(), nil, signingInfosInput{ConsensusAddress: "cosmosvalcons1abc"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getSigningInfos() single error = %v, toolError = %#v", err, toolResp.Error)
	}
	single, _ := toolResp.Data.(map[string]any)["val_signing_info"].(map[string]any)
	if single["address"] != "cosmosvalcons1abc" {
		t.Fatalf("val_signing_info = %#v", toolResp.Data)
	}
}

// The denom a user pastes is the full ibc/HASH voucher, but the transfer
// routes are keyed by the bare hash.
func TestGetDenomTraceAcceptsPrefixedAndBareHash(t *testing.T) {
	const hash = "27394FB092D2ECCD56123C74F36E4C1F926001CEADA9CA97EA622B25F41E5EB2"
	for _, denom := range []string{"ibc/" + hash, hash} {
		s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/ibc/apps/transfer/v1/denom_traces/"+hash {
				t.Errorf("unexpected path %s", r.URL.Path)
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"denom_trace": map[string]any{"path": "transfer/channel-0", "base_denom": "uatom"},
			})
		})
		_, toolResp, err := s.getDenomTrace(context.Background(), nil, denomTraceInput{Denom: denom})
		if err != nil || toolResp.Error != nil {
			t.Fatalf("getDenomTrace(%q) error = %v, toolError = %#v", denom, err, toolResp.Error)
		}
		trace, _ := toolResp.Data.(map[string]any)["denom_trace"].(map[string]any)
		if trace["base_denom"] != "uatom" {
			t.Fatalf("getDenomTrace(%q) = %#v", denom, toolResp.Data)
		}
	}
}

// ibc-go v9 renamed DenomTrace to Denom and /denom_traces to /denoms. A v9
// chain does not 404 the route it dropped - grpc-gateway still has a pattern
// for it and answers 501 with gRPC code 12 - so 501 is what this fallback has
// to survive in the field.
func TestGetDenomTraceFallsBackToIBCv9Route(t *testing.T) {
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ibc/apps/transfer/v1/denom_traces/ABC":
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`{"code":12, "message":"Not Implemented", "details":[]}`))
		case "/ibc/apps/transfer/v1/denoms/ABC":
			_ = json.NewEncoder(w).Encode(map[string]any{"denom": map[string]any{"base": "uatom"}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	_, toolResp, err := s.getDenomTrace(context.Background(), nil, denomTraceInput{Denom: "ibc/ABC"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getDenomTrace() error = %v, toolError = %#v", err, toolResp.Error)
	}
	if toolResp.Meta.Binding != "/ibc/apps/transfer/v1/denoms/ABC" {
		t.Fatalf("binding = %s", toolResp.Meta.Binding)
	}
}

func TestGetInflationWarnsWhenAnnualProvisionsMissing(t *testing.T) {
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/mint/v1beta1/inflation":
			_ = json.NewEncoder(w).Encode(map[string]any{"inflation": "0.130000000000000000"})
		case "/cosmos/mint/v1beta1/annual_provisions":
			http.NotFound(w, r)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	_, toolResp, err := s.getInflation(context.Background(), nil, heightInput{})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getInflation() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	if data["inflation"] != "0.130000000000000000" {
		t.Fatalf("inflation = %#v", data["inflation"])
	}
	warnings, _ := data["warnings"].([]string)
	if len(warnings) != 1 || warnings[0] != "annual provisions are unavailable" {
		t.Fatalf("warnings = %#v", data["warnings"])
	}
}

// A chain that replaced the standard x/mint exposes neither query; that is an
// unsupported capability, not a transport failure.
func TestGetInflationUnsupportedWhenMintAbsent(t *testing.T) {
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	_, toolResp, err := s.getInflation(context.Background(), nil, heightInput{})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeUnsupportedCapability {
		t.Fatalf("getInflation() = %#v, err=%v", toolResp.Error, err)
	}
}

func TestModuleParamsRouteToTheirOwnModule(t *testing.T) {
	for _, module := range []string{"staking", "distribution", "mint", "slashing", "bank"} {
		s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/cosmos/"+module+"/v1beta1/params" {
				t.Errorf("%s params requested %s", module, r.URL.Path)
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"params": map[string]any{"module": module}})
		})
		_, toolResp, err := s.moduleParams(module)(context.Background(), nil, heightInput{})
		if err != nil || toolResp.Error != nil {
			t.Fatalf("%s params error = %v, toolError = %#v", module, err, toolResp.Error)
		}
		params, _ := toolResp.Data.(map[string]any)["params"].(map[string]any)
		if params["module"] != module {
			t.Fatalf("%s params = %#v", module, toolResp.Data)
		}
	}
}

// x/gov keys its parameters by type, so the route carries a params_type
// segment that the other modules do not have. gov v1 answers with the whole
// set in "params" regardless of the type asked for, so one request suffices.
func TestGetGovParamsReadsV1InOneRequest(t *testing.T) {
	var requests int
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/cosmos/gov/v1/params/voting" {
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"voting_params": map[string]any{"voting_period": "172800s"},
			"params":        map[string]any{"quorum": "0.334000000000000000"},
		})
	})
	_, toolResp, err := s.getGovParams(context.Background(), nil, heightInput{})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getGovParams() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	voting, _ := data["voting_params"].(map[string]any)
	params, _ := data["params"].(map[string]any)
	if voting["voting_period"] != "172800s" || params["quorum"] != "0.334000000000000000" {
		t.Fatalf("gov params = %#v", data)
	}
	if requests != 1 {
		t.Fatalf("v1 answered the whole set but the tool made %d requests", requests)
	}
}

// v1beta1 has no "params" field and populates only the group named, so asking
// it for "voting" alone would report the voting period and silently omit the
// quorum and deposit parameters this tool advertises.
func TestGetGovParamsCompletesV1beta1FromEachGroup(t *testing.T) {
	bodies := map[string]map[string]any{
		"voting":   {"voting_params": map[string]any{"voting_period": "172800s"}},
		"deposit":  {"deposit_params": map[string]any{"min_deposit": []any{map[string]any{"denom": "stake", "amount": "10"}}}},
		"tallying": {"tally_params": map[string]any{"quorum": "0.334000000000000000"}},
	}
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		group, ok := strings.CutPrefix(r.URL.Path, "/cosmos/gov/v1beta1/params/")
		if !ok || bodies[group] == nil {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(bodies[group])
	})
	_, toolResp, err := s.getGovParams(context.Background(), nil, heightInput{})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getGovParams() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	voting, _ := data["voting_params"].(map[string]any)
	deposit, _ := data["deposit_params"].(map[string]any)
	tally, _ := data["tally_params"].(map[string]any)
	if voting["voting_period"] != "172800s" {
		t.Fatalf("voting_params = %#v", data["voting_params"])
	}
	if deposit["min_deposit"] == nil {
		t.Fatalf("deposit_params missing; v1beta1 needs its own query: %#v", data)
	}
	if tally["quorum"] != "0.334000000000000000" {
		t.Fatalf("tally_params missing; v1beta1 needs its own query: %#v", data)
	}
	if toolResp.Meta.Binding == "" || !strings.Contains(toolResp.Meta.Binding, ";") {
		t.Fatalf("binding should name every route that answered, got %q", toolResp.Meta.Binding)
	}
}

// A group the chain will not serve degrades to a warning rather than losing the
// parameters that did resolve.
func TestGetGovParamsWarnsOnUnavailableGroup(t *testing.T) {
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cosmos/gov/v1beta1/params/voting" {
			_ = json.NewEncoder(w).Encode(map[string]any{"voting_params": map[string]any{"voting_period": "172800s"}})
			return
		}
		http.NotFound(w, r)
	})
	_, toolResp, err := s.getGovParams(context.Background(), nil, heightInput{})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getGovParams() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	warnings, _ := data["warnings"].([]string)
	if len(warnings) != 2 {
		t.Fatalf("warnings = %#v", data["warnings"])
	}
	if voting, _ := data["voting_params"].(map[string]any); voting["voting_period"] != "172800s" {
		t.Fatalf("a missing group must not discard the group that resolved: %#v", data)
	}
}

func TestGetRedelegationsPassesValidatorFilters(t *testing.T) {
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.URL.Path != "/cosmos/staking/v1beta1/delegators/cosmos1abc/redelegations" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if query.Get("src_validator_addr") != "cosmosvaloper1src" || query.Get("dst_validator_addr") != "cosmosvaloper1dst" {
			t.Errorf("validator filters = %s", r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"redelegation_responses": []any{map[string]any{"redelegation": map[string]any{}}},
			"pagination":             map[string]any{"next_key": nil, "total": "1"},
		})
	})
	_, toolResp, err := s.getRedelegations(context.Background(), nil, redelegationsInput{
		DelegatorAddress:    "cosmos1abc",
		SrcValidatorAddress: "cosmosvaloper1src",
		DstValidatorAddress: "cosmosvaloper1dst",
	})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getRedelegations() error = %v, toolError = %#v", err, toolResp.Error)
	}
	if responses, _ := toolResp.Data.(map[string]any)["redelegation_responses"].([]any); len(responses) != 1 {
		t.Fatalf("redelegation_responses = %#v", toolResp.Data)
	}
}

func TestGetValidatorDelegationsAndStakingPoolAndTotalSupply(t *testing.T) {
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/staking/v1beta1/validators/cosmosvaloper1abc/delegations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"delegation_responses": []any{map[string]any{"delegation": map[string]any{}}},
				"pagination":           map[string]any{"next_key": nil, "total": "1"},
			})
		case "/cosmos/staking/v1beta1/pool":
			_ = json.NewEncoder(w).Encode(map[string]any{"pool": map[string]any{"bonded_tokens": "100"}})
		case "/cosmos/bank/v1beta1/supply":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"supply":     []any{map[string]any{"denom": "stake", "amount": "1000"}},
				"pagination": map[string]any{"next_key": nil, "total": "1"},
			})
		case "/cosmos/distribution/v1beta1/community_pool":
			_ = json.NewEncoder(w).Encode(map[string]any{"pool": []any{map[string]any{"denom": "stake", "amount": "5"}}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	_, toolResp, err := s.getValidatorDelegations(context.Background(), nil, validatorDelegationsInput{ValidatorAddress: "cosmosvaloper1abc"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getValidatorDelegations() = %v / %#v", err, toolResp.Error)
	}

	_, toolResp, err = s.getStakingPool(context.Background(), nil, heightInput{})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getStakingPool() = %v / %#v", err, toolResp.Error)
	}
	pool, _ := toolResp.Data.(map[string]any)["pool"].(map[string]any)
	if pool["bonded_tokens"] != "100" {
		t.Fatalf("pool = %#v", toolResp.Data)
	}

	_, toolResp, err = s.getTotalSupply(context.Background(), nil, pagedInput{})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getTotalSupply() = %v / %#v", err, toolResp.Error)
	}
	if supply, _ := toolResp.Data.(map[string]any)["supply"].([]any); len(supply) != 1 {
		t.Fatalf("supply = %#v", toolResp.Data)
	}

	_, toolResp, err = s.getCommunityPool(context.Background(), nil, heightInput{})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getCommunityPool() = %v / %#v", err, toolResp.Error)
	}
}

func TestGetTokenInfoAttachesIBCDenomTrace(t *testing.T) {
	const hash = "ABC123"
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/bank/v1beta1/supply/by_denom":
			_ = json.NewEncoder(w).Encode(map[string]any{"amount": map[string]any{"denom": "ibc/" + hash, "amount": "10"}})
		case "/cosmos/bank/v1beta1/denoms_metadata/ibc/" + hash:
			http.NotFound(w, r)
		case "/ibc/apps/transfer/v1/denom_traces/" + hash:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"denom_trace": map[string]any{"path": "transfer/channel-0", "base_denom": "uatom"},
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	_, toolResp, err := s.getTokenInfo(context.Background(), nil, denomInput{Denom: "ibc/" + hash})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getTokenInfo() error = %v, toolError = %#v", err, toolResp.Error)
	}
	trace, _ := toolResp.Data.(map[string]any)["denom_trace"].(map[string]any)
	if trace["base_denom"] != "uatom" {
		t.Fatalf("denom_trace = %#v", toolResp.Data)
	}
}
