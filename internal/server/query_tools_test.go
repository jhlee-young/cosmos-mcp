package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/jhlee-young/cosmos-mcp/internal/cosmos"
)

func TestPaginationDefaultsAndLimits(t *testing.T) {
	grpc, lcd, err := pagination(nil)
	if err != nil || grpc["limit"] != "50" || lcd["pagination.limit"] != "50" {
		t.Fatalf("pagination(nil) = %#v, %#v, %v", grpc, lcd, err)
	}
	if _, _, err = pagination(&paginationInput{Limit: 201}); errorCodeForTest(err) != cosmos.CodeInvalidInput {
		t.Fatalf("large pagination error = %v", err)
	}
	if _, _, err = pagination(&paginationInput{Key: "key", Offset: 1}); errorCodeForTest(err) != cosmos.CodeInvalidInput {
		t.Fatalf("key and offset error = %v", err)
	}
}

func TestNormalizeListPreservesRawAndPage(t *testing.T) {
	raw := map[string]any{
		"balances":   []any{map[string]any{"denom": "stake", "amount": "10"}},
		"pagination": map[string]any{"next_key": "next", "total": "2"},
	}
	result := normalizeList(raw, "balances")
	page := result["pagination"].(map[string]any)
	if !reflect.DeepEqual(result["raw"], raw) || page["next_key"] != "next" || page["total"] != "2" {
		t.Fatalf("normalizeList() = %#v", result)
	}
}

func TestInputValidation(t *testing.T) {
	if _, err := safeDenom("ibc/ABC123"); err != nil {
		t.Fatalf("IBC denom rejected: %v", err)
	}
	if _, err := safeValue("cosmos1abc/def", "address"); errorCodeForTest(err) != cosmos.CodeInvalidInput {
		t.Fatalf("unsafe address error = %v", err)
	}
	if _, err := normalizedHash("xyz"); errorCodeForTest(err) != cosmos.CodeInvalidInput {
		t.Fatalf("invalid hash error = %v", err)
	}
}

func errorCodeForTest(err error) cosmos.ErrorCode {
	code, _ := cosmos.ErrorDetails(err)
	return code
}

func TestResolveChainStatusMergesLCDNodeInfoAndSyncing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/base/tendermint/v1beta1/node_info":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_node_info": map[string]any{"network": "test-1", "version": "1.2.3"}})
		case "/cosmos/base/tendermint/v1beta1/syncing":
			_ = json.NewEncoder(w).Encode(map[string]any{"syncing": false})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	resolved, err := s.resolveChainStatus(context.Background())
	if err != nil {
		t.Fatalf("resolveChainStatus() error = %v", err)
	}
	wantBinding := "/cosmos/base/tendermint/v1beta1/node_info;/cosmos/base/tendermint/v1beta1/syncing"
	if resolved.Source != "lcd" || resolved.Binding != wantBinding {
		t.Fatalf("resolveChainStatus() source/binding = %s / %s, want lcd / %s", resolved.Source, resolved.Binding, wantBinding)
	}
	data, ok := resolved.Data.(map[string]any)
	if !ok || data["chain_id"] != "test-1" || data["node_version"] != "1.2.3" || data["catching_up"] != false {
		t.Fatalf("resolveChainStatus() data = %#v", resolved.Data)
	}
}

func TestGetTokenInfoMergesSupplyAndMetadataViaLCD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/bank/v1beta1/supply/by_denom":
			if r.URL.Query().Get("denom") != "stake" {
				t.Fatalf("unexpected supply query %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"amount": map[string]any{"denom": "stake", "amount": "1000"}})
		case "/cosmos/bank/v1beta1/denoms_metadata/stake":
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"base": "stake", "display": "STAKE"}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	_, toolResp, err := s.getTokenInfo(context.Background(), nil, denomInput{Denom: "stake"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getTokenInfo() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, ok := toolResp.Data.(map[string]any)
	if !ok {
		t.Fatalf("getTokenInfo() data type = %T", toolResp.Data)
	}
	if _, exists := data["warnings"]; exists {
		t.Fatalf("getTokenInfo() unexpected warnings = %#v", data["warnings"])
	}
	supply, _ := data["supply"].(map[string]any)
	if supply["amount"] != "1000" {
		t.Fatalf("getTokenInfo() supply = %#v", data["supply"])
	}
	metadata, _ := data["metadata"].(map[string]any)
	if metadata["base"] != "stake" {
		t.Fatalf("getTokenInfo() metadata = %#v", data["metadata"])
	}
	if toolResp.Source != "lcd" {
		t.Fatalf("getTokenInfo() source = %s", toolResp.Source)
	}
}

func TestGetTokenInfoWarnsWhenMetadataUnavailable(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/bank/v1beta1/supply/by_denom":
			_ = json.NewEncoder(w).Encode(map[string]any{"amount": map[string]any{"denom": "stake", "amount": "1000"}})
		case "/cosmos/bank/v1beta1/denoms_metadata/stake":
			http.NotFound(w, r)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	_, toolResp, err := s.getTokenInfo(context.Background(), nil, denomInput{Denom: "stake"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getTokenInfo() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	warnings, _ := data["warnings"].([]string)
	if len(warnings) != 1 || warnings[0] != "denomination metadata is unavailable" {
		t.Fatalf("getTokenInfo() warnings = %#v", data["warnings"])
	}
	if _, exists := data["metadata"]; exists {
		t.Fatalf("getTokenInfo() unexpected metadata = %#v", data["metadata"])
	}
}

func TestSearchTransactionsRPCBindingUsesPagePagination(t *testing.T) {
	var gotPage, gotPerPage string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		params, _ := req["params"].(map[string]any)
		gotPage, _ = params["page"].(string)
		gotPerPage, _ = params["per_page"].(string)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": map[string]any{"txs": []any{}, "total_count": "0"}})
	}))
	defer upstream.Close()
	rpc, err := cosmos.NewRPCClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{rpc: rpc, query: cosmos.NewResolver(rpc, nil, nil)}

	_, toolResp, err := s.searchTransactions(context.Background(), nil, transactionSearchInput{
		Events:     []string{"message.sender='cosmos1abc'"},
		Pagination: &paginationInput{Offset: 100, Limit: 50},
	})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("searchTransactions() with aligned offset error = %v, toolError = %#v", err, toolResp.Error)
	}
	if toolResp.Source != "rpc" || gotPage != "3" || gotPerPage != "50" {
		t.Fatalf("searchTransactions() source=%s page=%s per_page=%s, want rpc/3/50", toolResp.Source, gotPage, gotPerPage)
	}

	_, toolResp, err = s.searchTransactions(context.Background(), nil, transactionSearchInput{
		Events:     []string{"message.sender='cosmos1abc'"},
		Pagination: &paginationInput{Offset: 30, Limit: 50},
	})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeInvalidInput {
		t.Fatalf("searchTransactions() with misaligned offset = %#v, err=%v, want invalid_input", toolResp.Error, err)
	}
	_, toolResp, err = s.searchTransactions(context.Background(), nil, transactionSearchInput{
		Events:     []string{"message.sender='cosmos1abc'"},
		Pagination: &paginationInput{Reverse: true},
	})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeInvalidInput {
		t.Fatalf("searchTransactions() with reverse pagination = %#v, err=%v, want invalid_input", toolResp.Error, err)
	}
	_, toolResp, err = s.searchTransactions(context.Background(), nil, transactionSearchInput{
		Events:     []string{"message.sender='cosmos1abc'"},
		Pagination: &paginationInput{Key: "next"},
	})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeInvalidInput {
		t.Fatalf("searchTransactions() with key pagination = %#v, err=%v, want invalid_input", toolResp.Error, err)
	}
}

func TestSearchTransactionsLCDSendsCurrentAndLegacyFields(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if got := query["events"]; !reflect.DeepEqual(got, []string{"message.sender='cosmos1abc'", "tx.height=10"}) {
			t.Fatalf("events = %v", got)
		}
		if query.Get("query") != "message.sender='cosmos1abc' AND tx.height=10" || query.Get("page") != "3" || query.Get("limit") != "50" {
			t.Fatalf("current query fields = %s", r.URL.RawQuery)
		}
		if query.Get("pagination.offset") != "100" || query.Get("pagination.limit") != "50" {
			t.Fatalf("legacy pagination fields = %s", r.URL.RawQuery)
		}
		if query.Get("order_by") != "ORDER_BY_DESC" {
			t.Fatalf("order_by = %q", query.Get("order_by"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"txs": []any{}, "tx_responses": []any{}, "total": "0"})
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	_, toolResp, err := s.searchTransactions(context.Background(), nil, transactionSearchInput{
		Events:     []string{"message.sender='cosmos1abc'", "tx.height=10"},
		Order:      "desc",
		Pagination: &paginationInput{Offset: 100, Limit: 50},
	})
	if err != nil || toolResp.Error != nil || toolResp.Source != "lcd" {
		t.Fatalf("searchTransactions() = %#v, %v", toolResp, err)
	}
}

func TestGetAccountViaLCD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cosmos/auth/v1beta1/accounts/cosmos1abc" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"account": map[string]any{"address": "cosmos1abc", "account_number": "5"}})
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	_, toolResp, err := s.getAccount(context.Background(), nil, addressInput{Address: "cosmos1abc"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getAccount() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	account, _ := data["account"].(map[string]any)
	if account["address"] != "cosmos1abc" || toolResp.Source != "lcd" {
		t.Fatalf("getAccount() = %#v", toolResp)
	}
}

func TestGetAccountRejectsUnsafeAddress(t *testing.T) {
	s := &Server{query: cosmos.NewResolver(nil, nil, nil)}
	_, toolResp, err := s.getAccount(context.Background(), nil, addressInput{Address: "cosmos1/abc"})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeInvalidInput {
		t.Fatalf("getAccount() with unsafe address = %#v, err=%v", toolResp.Error, err)
	}
}

func TestGetAccountUnsupportedWithoutEndpoints(t *testing.T) {
	s := &Server{query: cosmos.NewResolver(nil, nil, nil)}
	_, toolResp, err := s.getAccount(context.Background(), nil, addressInput{Address: "cosmos1abc"})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeUnsupportedCapability {
		t.Fatalf("getAccount() without endpoints = %#v, err=%v", toolResp.Error, err)
	}
}

func TestGetBalancesAllViaLCD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cosmos/bank/v1beta1/balances/cosmos1abc" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"balances":   []any{map[string]any{"denom": "stake", "amount": "10"}},
			"pagination": map[string]any{"next_key": nil, "total": "1"},
		})
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	_, toolResp, err := s.getBalances(context.Background(), nil, balancesInput{Address: "cosmos1abc"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getBalances() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	if balances, _ := data["balances"].([]any); len(balances) != 1 {
		t.Fatalf("getBalances() balances = %#v", data["balances"])
	}
}

func TestGetBalancesSingleDenomViaLCD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cosmos/bank/v1beta1/balances/cosmos1abc/by_denom" || r.URL.Query().Get("denom") != "stake" {
			t.Fatalf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"balance": map[string]any{"denom": "stake", "amount": "10"}})
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	_, toolResp, err := s.getBalances(context.Background(), nil, balancesInput{Address: "cosmos1abc", Denom: "stake"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getBalances() denom error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	balances, _ := data["balances"].([]any)
	if len(balances) != 1 {
		t.Fatalf("getBalances() single denom = %#v", data["balances"])
	}
	balance, _ := balances[0].(map[string]any)
	if balance["denom"] != "stake" {
		t.Fatalf("getBalances() single denom balance = %#v", balance)
	}
}

func TestGetBalancesRejectsOversizedPagination(t *testing.T) {
	s := &Server{query: cosmos.NewResolver(nil, nil, nil)}
	_, toolResp, err := s.getBalances(context.Background(), nil, balancesInput{Address: "cosmos1abc", Pagination: &paginationInput{Limit: 500}})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeInvalidInput {
		t.Fatalf("getBalances() with oversized pagination = %#v, err=%v", toolResp.Error, err)
	}
}

func TestGetValidatorsViaLCD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cosmos/staking/v1beta1/validators" || r.URL.Query().Get("status") != "BOND_STATUS_BONDED" {
			t.Fatalf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"validators": []any{map[string]any{"operator_address": "cosmosvaloper1abc"}},
			"pagination": map[string]any{"next_key": nil, "total": "1"},
		})
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	_, toolResp, err := s.getValidators(context.Background(), nil, validatorsInput{Status: "BOND_STATUS_BONDED"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getValidators() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	if validators, _ := data["validators"].([]any); len(validators) != 1 {
		t.Fatalf("getValidators() validators = %#v", data["validators"])
	}
}

func TestGetValidatorViaLCD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cosmos/staking/v1beta1/validators/cosmosvaloper1abc" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"validator": map[string]any{"operator_address": "cosmosvaloper1abc"}})
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	_, toolResp, err := s.getValidator(context.Background(), nil, validatorInput{ValidatorAddress: "cosmosvaloper1abc"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getValidator() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	validator, _ := data["validator"].(map[string]any)
	if validator["operator_address"] != "cosmosvaloper1abc" {
		t.Fatalf("getValidator() = %#v", data["validator"])
	}
}

func TestGetDelegationsAndUnbondingDelegationsViaLCD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/staking/v1beta1/delegations/cosmos1abc":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"delegation_responses": []any{map[string]any{"delegation": map[string]any{"delegator_address": "cosmos1abc"}}},
				"pagination":           map[string]any{"next_key": nil, "total": "1"},
			})
		case "/cosmos/staking/v1beta1/delegators/cosmos1abc/unbonding_delegations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"unbonding_responses": []any{map[string]any{"delegator_address": "cosmos1abc"}},
				"pagination":          map[string]any{"next_key": nil, "total": "1"},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}

	_, toolResp, err := s.getDelegations(context.Background(), nil, delegationsInput{DelegatorAddress: "cosmos1abc"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getDelegations() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	if responses, _ := data["delegation_responses"].([]any); len(responses) != 1 {
		t.Fatalf("getDelegations() = %#v", data["delegation_responses"])
	}

	_, toolResp, err = s.getUnbondingDelegations(context.Background(), nil, delegationsInput{DelegatorAddress: "cosmos1abc"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getUnbondingDelegations() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ = toolResp.Data.(map[string]any)
	if responses, _ := data["unbonding_responses"].([]any); len(responses) != 1 {
		t.Fatalf("getUnbondingDelegations() = %#v", data["unbonding_responses"])
	}
}

func TestGetRewardsTotalAndForValidatorViaLCD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/distribution/v1beta1/delegators/cosmos1abc/rewards":
			_ = json.NewEncoder(w).Encode(map[string]any{"rewards": []any{}, "total": []any{map[string]any{"denom": "stake", "amount": "1"}}})
		case "/cosmos/distribution/v1beta1/delegators/cosmos1abc/rewards/cosmosvaloper1abc":
			_ = json.NewEncoder(w).Encode(map[string]any{"rewards": []any{map[string]any{"denom": "stake", "amount": "1"}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}

	_, toolResp, err := s.getRewards(context.Background(), nil, rewardsInput{DelegatorAddress: "cosmos1abc"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getRewards() total error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	if _, ok := data["total"]; !ok {
		t.Fatalf("getRewards() total = %#v", data)
	}

	_, toolResp, err = s.getRewards(context.Background(), nil, rewardsInput{DelegatorAddress: "cosmos1abc", ValidatorAddress: "cosmosvaloper1abc"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getRewards() validator error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ = toolResp.Data.(map[string]any)
	if rewards, _ := data["rewards"].([]any); len(rewards) != 1 {
		t.Fatalf("getRewards() validator rewards = %#v", data["rewards"])
	}
}

func TestGetProposalsFallsBackFromV1ToV1beta1ViaLCD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/gov/v1/proposals":
			http.NotFound(w, r)
		case "/cosmos/gov/v1beta1/proposals":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"proposals":  []any{map[string]any{"id": "1"}},
				"pagination": map[string]any{"next_key": nil, "total": "1"},
			})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	_, toolResp, err := s.getProposals(context.Background(), nil, proposalsInput{})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getProposals() error = %v, toolError = %#v", err, toolResp.Error)
	}
	if toolResp.Source != "lcd" || toolResp.Meta.Binding != "/cosmos/gov/v1beta1/proposals" {
		t.Fatalf("getProposals() source/binding = %s/%s", toolResp.Source, toolResp.Meta.Binding)
	}
	data, _ := toolResp.Data.(map[string]any)
	if proposals, _ := data["proposals"].([]any); len(proposals) != 1 {
		t.Fatalf("getProposals() proposals = %#v", data["proposals"])
	}
}

func TestGetProposalViaLCDAndInvalidID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cosmos/gov/v1/proposals/1" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"proposal": map[string]any{"id": "1"}})
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	_, toolResp, err := s.getProposal(context.Background(), nil, proposalInput{ProposalID: "1"})
	if err != nil || toolResp.Error != nil {
		t.Fatalf("getProposal() error = %v, toolError = %#v", err, toolResp.Error)
	}
	data, _ := toolResp.Data.(map[string]any)
	proposal, _ := data["proposal"].(map[string]any)
	if proposal["id"] != "1" {
		t.Fatalf("getProposal() = %#v", data["proposal"])
	}

	_, toolResp, err = s.getProposal(context.Background(), nil, proposalInput{ProposalID: "abc"})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeInvalidInput {
		t.Fatalf("getProposal() invalid id = %#v, err=%v", toolResp.Error, err)
	}
}

func TestSimulateTransactionValidation(t *testing.T) {
	s := &Server{query: cosmos.NewResolver(nil, nil, nil)}
	_, toolResp, err := s.simulateTransaction(context.Background(), nil, simulationInput{})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeInvalidInput {
		t.Fatalf("simulateTransaction() empty tx_bytes = %#v, err=%v", toolResp.Error, err)
	}
	_, toolResp, err = s.simulateTransaction(context.Background(), nil, simulationInput{TxBytes: "not-base64!!"})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeInvalidInput {
		t.Fatalf("simulateTransaction() invalid base64 = %#v, err=%v", toolResp.Error, err)
	}
}

func TestSimulateTransactionUnsupportedWithoutGRPC(t *testing.T) {
	s := &Server{query: cosmos.NewResolver(nil, nil, nil)}
	txBytes := base64.StdEncoding.EncodeToString([]byte{0x01, 0x02})
	_, toolResp, err := s.simulateTransaction(context.Background(), nil, simulationInput{TxBytes: txBytes})
	if err != nil || toolResp.Error == nil || toolResp.Error.Code != cosmos.CodeUnsupportedCapability {
		t.Fatalf("simulateTransaction() without gRPC = %#v, err=%v", toolResp.Error, err)
	}
}

func TestResolveBlockLatestAndHeightViaLCD(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cosmos/base/tendermint/v1beta1/blocks/latest":
			_ = json.NewEncoder(w).Encode(map[string]any{"block": map[string]any{"header": map[string]any{"height": "100"}}})
		case "/cosmos/base/tendermint/v1beta1/blocks/42":
			_ = json.NewEncoder(w).Encode(map[string]any{"block": map[string]any{"header": map[string]any{"height": "42"}}})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}

	resolved, err := s.resolveBlock(context.Background(), "")
	if err != nil {
		t.Fatalf("resolveBlock(latest) error = %v", err)
	}
	if height := blockHeight(resolved.Data); height != "100" {
		t.Fatalf("resolveBlock(latest) height = %v, data = %#v", height, resolved.Data)
	}

	resolved, err = s.resolveBlock(context.Background(), "42")
	if err != nil {
		t.Fatalf("resolveBlock(42) error = %v", err)
	}
	if height := blockHeight(resolved.Data); height != "42" {
		t.Fatalf("resolveBlock(42) height = %v, data = %#v", height, resolved.Data)
	}

	if _, err = s.resolveBlock(context.Background(), "not-a-number"); errorCodeForTest(err) != cosmos.CodeInvalidInput {
		t.Fatalf("resolveBlock(invalid) error = %v", err)
	}
	if _, err = s.resolveBlock(context.Background(), "0"); errorCodeForTest(err) != cosmos.CodeInvalidInput {
		t.Fatalf("resolveBlock(zero) error = %v", err)
	}
}

func blockHeight(data any) any {
	block, _ := data.(map[string]any)["block"].(map[string]any)
	header, _ := block["header"].(map[string]any)
	return header["height"]
}

func TestResolveTransactionViaLCDAndInvalidHash(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cosmos/tx/v1beta1/txs/ABCDEF12" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tx_response": map[string]any{"txhash": "ABCDEF12"}})
	}))
	defer upstream.Close()
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
	resolved, err := s.resolveTransaction(context.Background(), "abcdef12")
	if err != nil {
		t.Fatalf("resolveTransaction() error = %v", err)
	}
	data, _ := resolved.Data.(map[string]any)
	txResponse, _ := data["tx_response"].(map[string]any)
	if txResponse["txhash"] != "ABCDEF12" {
		t.Fatalf("resolveTransaction() = %#v", resolved.Data)
	}

	if _, err = s.resolveTransaction(context.Background(), "xyz"); errorCodeForTest(err) != cosmos.CodeInvalidInput {
		t.Fatalf("resolveTransaction(invalid hash) error = %v", err)
	}
}
