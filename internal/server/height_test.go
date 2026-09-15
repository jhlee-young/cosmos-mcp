package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jhlee-young/cosmos-mcp/internal/cosmos"
)

// Every state query must reach the endpoint with the height pinned; a tool that
// silently drops it would answer from current state while the caller believes
// it is reading history.
func TestStateToolsPinHeightOnTheUpstreamRequest(t *testing.T) {
	var gotHeight string
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotHeight = r.Header.Get(cosmos.HeightHeader)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account":              map[string]any{},
			"balances":             []any{},
			"validators":           []any{},
			"validator":            map[string]any{},
			"delegation_responses": []any{},
			"proposal":             map[string]any{},
			"pagination":           map[string]any{},
		})
	})

	calls := map[string]func() error{
		"get_account": func() error {
			_, _, err := s.getAccount(t.Context(), nil, addressInput{Address: "cosmos1abc", heightInput: heightInput{Height: "100"}})
			return err
		},
		"get_balances": func() error {
			_, _, err := s.getBalances(t.Context(), nil, balancesInput{Address: "cosmos1abc", heightInput: heightInput{Height: "100"}})
			return err
		},
		"get_validators": func() error {
			_, _, err := s.getValidators(t.Context(), nil, validatorsInput{heightInput: heightInput{Height: "100"}})
			return err
		},
		"get_validator": func() error {
			_, _, err := s.getValidator(t.Context(), nil, validatorInput{ValidatorAddress: "cosmosvaloper1abc", heightInput: heightInput{Height: "100"}})
			return err
		},
		"get_delegations": func() error {
			_, _, err := s.getDelegations(t.Context(), nil, delegationsInput{DelegatorAddress: "cosmos1abc", heightInput: heightInput{Height: "100"}})
			return err
		},
		"get_rewards": func() error {
			_, _, err := s.getRewards(t.Context(), nil, rewardsInput{DelegatorAddress: "cosmos1abc", heightInput: heightInput{Height: "100"}})
			return err
		},
		"get_proposal": func() error {
			_, _, err := s.getProposal(t.Context(), nil, proposalInput{ProposalID: "1", heightInput: heightInput{Height: "100"}})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			gotHeight = ""
			if err := call(); err != nil {
				t.Fatal(err)
			}
			if gotHeight != "100" {
				t.Fatalf("%s sent %s = %q, want 100", name, cosmos.HeightHeader, gotHeight)
			}
		})
	}
}

func TestStateToolsRejectInvalidHeight(t *testing.T) {
	s := newLCDServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{}})
	})
	for _, height := range []string{"abc", "0", "-5", "1.5", " "} {
		_, toolResp, err := s.getAccount(context.Background(), nil, addressInput{Address: "cosmos1abc", heightInput: heightInput{Height: height}})
		if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeInvalidInput {
			t.Fatalf("getAccount(height=%q) = %#v, err=%v", height, toolResp.Error, err)
		}
	}
}

func TestStateToolsOmitHeightHeaderByDefault(t *testing.T) {
	var present bool
	s := newLCDServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, present = r.Header[http.CanonicalHeaderKey(cosmos.HeightHeader)]
		_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{}})
	})
	if _, _, err := s.getAccount(context.Background(), nil, addressInput{Address: "cosmos1abc"}); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatalf("%s must not be sent when no height is requested", cosmos.HeightHeader)
	}
}
