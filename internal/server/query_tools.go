package server

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jhlee-young/cosmos-mcp/internal/cosmos"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	defaultPageLimit = uint64(50)
	maxPageLimit     = uint64(200)
)

func (s *Server) resolveChainStatus(ctx context.Context) (cosmos.Resolution, error) {
	resolved, err := s.query.Resolve(ctx, "chain_status", []cosmos.Binding{
		{Name: "rpc:status", RPCMethod: "status", RPCParams: map[string]any{}},
		{Name: "grpc:node_info", GRPCMethod: "/cosmos.base.tendermint.v1beta1.Service/GetNodeInfo", GRPCRequest: map[string]any{}},
		{Name: "lcd:node_info", LCDPath: "/cosmos/base/tendermint/v1beta1/node_info"},
	})
	if err != nil {
		return resolved, err
	}
	if resolved.Source == "rpc" {
		resolved.Data = withRaw(summarizeChainStatus(resolved.Data), resolved.Data)
		return resolved, nil
	}
	nodeRaw := resolved.Data
	syncBindings := []cosmos.Binding{}
	if resolved.Source == "grpc" {
		syncBindings = append(syncBindings, cosmos.Binding{Name: "grpc:syncing", GRPCMethod: "/cosmos.base.tendermint.v1beta1.Service/GetSyncing", GRPCRequest: map[string]any{}})
	} else {
		syncBindings = append(syncBindings, cosmos.Binding{Name: "lcd:syncing", LCDPath: "/cosmos/base/tendermint/v1beta1/syncing"})
	}
	syncResult, syncErr := s.query.Resolve(ctx, "chain_sync:"+resolved.Source, syncBindings)
	data := normalizeNodeStatus(nodeRaw, nil)
	if syncErr == nil {
		data = normalizeNodeStatus(nodeRaw, syncResult.Data)
		resolved.Binding += ";" + syncResult.Binding
	} else {
		data["warnings"] = []string{"sync status is unavailable"}
	}
	data["raw"] = map[string]any{"node_info": nodeRaw, "syncing": syncResult.Data}
	resolved.Data = data
	return resolved, nil
}

func (s *Server) resolveBlock(ctx context.Context, height string) (cosmos.Resolution, error) {
	request := map[string]any{}
	rpcParams := map[string]any{}
	grpcMethod := "/cosmos.base.tendermint.v1beta1.Service/GetLatestBlock"
	lcdPath := "/cosmos/base/tendermint/v1beta1/blocks/latest"
	if height != "" {
		value, err := strconv.ParseUint(height, 10, 64)
		if err != nil || value == 0 {
			return cosmos.Resolution{}, cosmos.NewError(cosmos.CodeInvalidInput, "block height must be a positive decimal string", err)
		}
		request["height"] = height
		rpcParams["height"] = height
		grpcMethod = "/cosmos.base.tendermint.v1beta1.Service/GetBlockByHeight"
		lcdPath = "/cosmos/base/tendermint/v1beta1/blocks/" + height
	}
	resolved, err := s.query.Resolve(ctx, "block:"+boolKey(height != ""), []cosmos.Binding{
		{Name: "rpc:block", RPCMethod: "block", RPCParams: rpcParams},
		{Name: "grpc:block", GRPCMethod: grpcMethod, GRPCRequest: request},
		{Name: "lcd:block", LCDPath: lcdPath},
	})
	if err == nil {
		resolved.Data = normalizeFields(resolved.Data, "block_id", "block", "sdk_block")
	}
	return resolved, err
}

func (s *Server) resolveTransaction(ctx context.Context, hash string) (cosmos.Resolution, error) {
	normalized, err := normalizedHash(hash)
	if err != nil {
		return cosmos.Resolution{}, err
	}
	resolved, err := s.query.Resolve(ctx, "transaction", []cosmos.Binding{
		{Name: "grpc:get_tx", GRPCMethod: "/cosmos.tx.v1beta1.Service/GetTx", GRPCRequest: map[string]any{"hash": normalized}},
		{Name: "lcd:get_tx", LCDPath: "/cosmos/tx/v1beta1/txs/" + url.PathEscape(normalized)},
		{Name: "rpc:tx", RPCMethod: "tx", RPCParams: map[string]any{"hash": "0x" + normalized, "prove": false}},
	})
	if err == nil {
		resolved.Data = normalizeFields(resolved.Data, "tx", "tx_response", "tx_result", "hash", "height")
	}
	return resolved, err
}

func (s *Server) getAccount(ctx context.Context, _ *mcp.CallToolRequest, in addressInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	address, err := safeValue(in.Address, "address")
	if err != nil {
		return response(started, "system", map[string]any{"address": in.Address}, nil, err)
	}
	resolved, err := s.query.Resolve(ctx, "account", []cosmos.Binding{
		{Name: "grpc:auth_account", GRPCMethod: "/cosmos.auth.v1beta1.Query/Account", GRPCRequest: map[string]any{"address": address}},
		{Name: "lcd:auth_account", LCDPath: "/cosmos/auth/v1beta1/accounts/" + url.PathEscape(address)},
	})
	if err == nil {
		resolved.Data = normalizeFields(resolved.Data, "account")
	}
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{"address": address}, resolved.Data, err)
}

func (s *Server) resolveBalances(ctx context.Context, in balancesInput) (cosmos.Resolution, error) {
	address, err := safeValue(in.Address, "address")
	if err != nil {
		return cosmos.Resolution{}, err
	}
	if in.Denom != "" {
		denom, valueErr := safeDenom(in.Denom)
		if valueErr != nil {
			return cosmos.Resolution{}, valueErr
		}
		resolved, resolveErr := s.query.Resolve(ctx, "balance", []cosmos.Binding{
			{Name: "grpc:balance", GRPCMethod: "/cosmos.bank.v1beta1.Query/Balance", GRPCRequest: map[string]any{"address": address, "denom": denom}},
			{Name: "lcd:balance", LCDPath: "/cosmos/bank/v1beta1/balances/" + url.PathEscape(address) + "/by_denom", LCDQuery: map[string]string{"denom": denom}},
		})
		if resolveErr == nil {
			raw := resolved.Data
			balances := []any{}
			if balance, ok := object(raw)["balance"]; ok && balance != nil {
				balances = append(balances, balance)
			}
			resolved.Data = map[string]any{"balances": balances, "raw": raw}
		}
		return resolved, resolveErr
	}
	grpcPage, lcdPage, err := pagination(in.Pagination)
	if err != nil {
		return cosmos.Resolution{}, err
	}
	resolved, err := s.query.Resolve(ctx, "balances", []cosmos.Binding{
		{Name: "grpc:all_balances", GRPCMethod: "/cosmos.bank.v1beta1.Query/AllBalances", GRPCRequest: map[string]any{"address": address, "pagination": grpcPage}},
		{Name: "lcd:all_balances", LCDPath: "/cosmos/bank/v1beta1/balances/" + url.PathEscape(address), LCDQuery: lcdPage},
	})
	if err == nil {
		resolved.Data = normalizeList(resolved.Data, "balances")
	}
	return resolved, err
}

func (s *Server) getTokenInfo(ctx context.Context, _ *mcp.CallToolRequest, in denomInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	denom, err := safeDenom(in.Denom)
	if err != nil {
		return response(started, "system", map[string]any{"denom": in.Denom}, nil, err)
	}
	supply, supplyErr := s.query.Resolve(ctx, "token_supply", []cosmos.Binding{
		{Name: "grpc:supply", GRPCMethod: "/cosmos.bank.v1beta1.Query/SupplyOf", GRPCRequest: map[string]any{"denom": denom}},
		{Name: "lcd:supply", LCDPath: "/cosmos/bank/v1beta1/supply/by_denom", LCDQuery: map[string]string{"denom": denom}},
	})
	if supplyErr != nil && !optionalCapabilityError(supplyErr) {
		return responseBound(started, supply.Source, supply.Binding, map[string]any{"denom": denom}, nil, supplyErr)
	}
	metadata, metadataErr := s.query.Resolve(ctx, "token_metadata", []cosmos.Binding{
		{Name: "grpc:metadata", GRPCMethod: "/cosmos.bank.v1beta1.Query/DenomMetadata", GRPCRequest: map[string]any{"denom": denom}},
		{Name: "lcd:metadata", LCDPath: "/cosmos/bank/v1beta1/denoms_metadata/" + url.PathEscape(denom)},
	})
	if metadataErr != nil && !optionalCapabilityError(metadataErr) {
		return responseBound(started, metadata.Source, metadata.Binding, map[string]any{"denom": denom}, nil, metadataErr)
	}
	if supplyErr != nil && metadataErr != nil {
		return response(started, "system", map[string]any{"denom": denom}, nil, supplyErr)
	}
	data := map[string]any{"denom": denom, "raw": map[string]any{}}
	warnings := []string{}
	sources, bindings := []string{}, []string{}
	if supplyErr == nil {
		data["supply"] = object(supply.Data)["amount"]
		data["raw"].(map[string]any)["supply"] = supply.Data
		sources, bindings = append(sources, supply.Source), append(bindings, supply.Binding)
	} else {
		warnings = append(warnings, "token supply is unavailable")
	}
	if metadataErr == nil {
		data["metadata"] = object(metadata.Data)["metadata"]
		data["raw"].(map[string]any)["metadata"] = metadata.Data
		sources, bindings = append(sources, metadata.Source), append(bindings, metadata.Binding)
	} else {
		warnings = append(warnings, "denomination metadata is unavailable")
	}
	if len(warnings) > 0 {
		data["warnings"] = warnings
	}
	return responseBound(started, commonSource(sources), strings.Join(bindings, ";"), map[string]any{"denom": denom}, data, nil)
}

func (s *Server) getValidators(ctx context.Context, _ *mcp.CallToolRequest, in validatorsInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	grpcPage, lcdPage, err := pagination(in.Pagination)
	if err != nil {
		return response(started, "system", map[string]any{}, nil, err)
	}
	request := map[string]any{"pagination": grpcPage}
	if in.Status != "" {
		request["status"] = in.Status
		lcdPage["status"] = in.Status
	}
	resolved, err := s.query.Resolve(ctx, "staking_validators", []cosmos.Binding{
		{Name: "grpc:staking_validators", GRPCMethod: "/cosmos.staking.v1beta1.Query/Validators", GRPCRequest: request},
		{Name: "lcd:staking_validators", LCDPath: "/cosmos/staking/v1beta1/validators", LCDQuery: lcdPage},
	})
	if err == nil {
		resolved.Data = normalizeList(resolved.Data, "validators")
	}
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{"status": in.Status, "pagination": in.Pagination}, resolved.Data, err)
}

func (s *Server) getValidator(ctx context.Context, _ *mcp.CallToolRequest, in validatorInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	address, err := safeValue(in.ValidatorAddress, "validator_address")
	if err != nil {
		return response(started, "system", map[string]any{}, nil, err)
	}
	resolved, err := s.query.Resolve(ctx, "staking_validator", []cosmos.Binding{
		{Name: "grpc:staking_validator", GRPCMethod: "/cosmos.staking.v1beta1.Query/Validator", GRPCRequest: map[string]any{"validator_addr": address}},
		{Name: "lcd:staking_validator", LCDPath: "/cosmos/staking/v1beta1/validators/" + url.PathEscape(address)},
	})
	if err == nil {
		resolved.Data = normalizeFields(resolved.Data, "validator")
	}
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{"validator_address": address}, resolved.Data, err)
}

func (s *Server) getDelegations(ctx context.Context, _ *mcp.CallToolRequest, in delegationsInput) (*mcp.CallToolResult, ToolResponse, error) {
	return s.delegationQuery(ctx, in, false)
}

func (s *Server) getUnbondingDelegations(ctx context.Context, _ *mcp.CallToolRequest, in delegationsInput) (*mcp.CallToolResult, ToolResponse, error) {
	return s.delegationQuery(ctx, in, true)
}

func (s *Server) delegationQuery(ctx context.Context, in delegationsInput, unbonding bool) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	address, err := safeValue(in.DelegatorAddress, "delegator_address")
	if err != nil {
		return response(started, "system", map[string]any{}, nil, err)
	}
	grpcPage, lcdPage, err := pagination(in.Pagination)
	if err != nil {
		return response(started, "system", map[string]any{}, nil, err)
	}
	capability, method, path, key := "delegations", "DelegatorDelegations", "/cosmos/staking/v1beta1/delegations/", "delegation_responses"
	if unbonding {
		capability, method, path, key = "unbonding_delegations", "DelegatorUnbondingDelegations", "/cosmos/staking/v1beta1/delegators/", "unbonding_responses"
		path += url.PathEscape(address) + "/unbonding_delegations"
	} else {
		path += url.PathEscape(address)
	}
	resolved, err := s.query.Resolve(ctx, capability, []cosmos.Binding{
		{Name: "grpc:" + capability, GRPCMethod: "/cosmos.staking.v1beta1.Query/" + method, GRPCRequest: map[string]any{"delegator_addr": address, "pagination": grpcPage}},
		{Name: "lcd:" + capability, LCDPath: path, LCDQuery: lcdPage},
	})
	if err == nil {
		resolved.Data = normalizeList(resolved.Data, key)
	}
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{"delegator_address": address, "pagination": in.Pagination}, resolved.Data, err)
}

func (s *Server) getRewards(ctx context.Context, _ *mcp.CallToolRequest, in rewardsInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	delegator, err := safeValue(in.DelegatorAddress, "delegator_address")
	if err != nil {
		return response(started, "system", map[string]any{}, nil, err)
	}
	method, path, capability := "DelegationTotalRewards", "/cosmos/distribution/v1beta1/delegators/"+url.PathEscape(delegator)+"/rewards", "total_rewards"
	request := map[string]any{"delegator_address": delegator}
	keys := []string{"rewards", "total"}
	if in.ValidatorAddress != "" {
		validator, valueErr := safeValue(in.ValidatorAddress, "validator_address")
		if valueErr != nil {
			return response(started, "system", map[string]any{}, nil, valueErr)
		}
		method, capability = "DelegationRewards", "delegation_rewards"
		request["validator_address"] = validator
		path += "/" + url.PathEscape(validator)
		keys = []string{"rewards"}
	}
	resolved, err := s.query.Resolve(ctx, capability, []cosmos.Binding{
		{Name: "grpc:" + capability, GRPCMethod: "/cosmos.distribution.v1beta1.Query/" + method, GRPCRequest: request},
		{Name: "lcd:" + capability, LCDPath: path},
	})
	if err == nil {
		resolved.Data = normalizeFields(resolved.Data, keys...)
	}
	return responseBound(started, resolved.Source, resolved.Binding, request, resolved.Data, err)
}

func (s *Server) getProposals(ctx context.Context, _ *mcp.CallToolRequest, in proposalsInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	grpcPage, lcdPage, err := pagination(in.Pagination)
	if err != nil {
		return response(started, "system", map[string]any{}, nil, err)
	}
	request := map[string]any{"pagination": grpcPage}
	if in.Status != "" {
		request["proposal_status"] = in.Status
		lcdPage["proposal_status"] = in.Status
	}
	if in.Voter != "" {
		request["voter"] = in.Voter
		lcdPage["voter"] = in.Voter
	}
	if in.Depositor != "" {
		request["depositor"] = in.Depositor
		lcdPage["depositor"] = in.Depositor
	}
	resolved, err := s.query.Resolve(ctx, "governance_proposals", []cosmos.Binding{
		{Name: "grpc:gov_v1_proposals", GRPCMethod: "/cosmos.gov.v1.Query/Proposals", GRPCRequest: request, GRPCDiscardUnknown: true},
		{Name: "grpc:gov_v1beta1_proposals", GRPCMethod: "/cosmos.gov.v1beta1.Query/Proposals", GRPCRequest: request, GRPCDiscardUnknown: true},
		{Name: "lcd:gov_v1_proposals", LCDPath: "/cosmos/gov/v1/proposals", LCDQuery: lcdPage},
		{Name: "lcd:gov_v1beta1_proposals", LCDPath: "/cosmos/gov/v1beta1/proposals", LCDQuery: lcdPage},
	})
	if err == nil {
		resolved.Data = normalizeList(resolved.Data, "proposals")
	}
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{"status": in.Status, "voter": in.Voter, "depositor": in.Depositor, "pagination": in.Pagination}, resolved.Data, err)
}

func (s *Server) getProposal(ctx context.Context, _ *mcp.CallToolRequest, in proposalInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	if value, err := strconv.ParseUint(in.ProposalID, 10, 64); err != nil || value == 0 {
		return response(started, "system", map[string]any{"proposal_id": in.ProposalID}, nil, cosmos.NewError(cosmos.CodeInvalidInput, "proposal_id must be a positive decimal string", err))
	}
	request := map[string]any{"proposal_id": in.ProposalID}
	resolved, err := s.query.Resolve(ctx, "governance_proposal", []cosmos.Binding{
		{Name: "grpc:gov_v1_proposal", GRPCMethod: "/cosmos.gov.v1.Query/Proposal", GRPCRequest: request},
		{Name: "grpc:gov_v1beta1_proposal", GRPCMethod: "/cosmos.gov.v1beta1.Query/Proposal", GRPCRequest: request},
		{Name: "lcd:gov_v1_proposal", LCDPath: "/cosmos/gov/v1/proposals/" + in.ProposalID},
		{Name: "lcd:gov_v1beta1_proposal", LCDPath: "/cosmos/gov/v1beta1/proposals/" + in.ProposalID},
	})
	if err == nil {
		resolved.Data = normalizeFields(resolved.Data, "proposal")
	}
	return responseBound(started, resolved.Source, resolved.Binding, request, resolved.Data, err)
}

func (s *Server) searchTransactions(ctx context.Context, _ *mcp.CallToolRequest, in transactionSearchInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	if len(in.Events) == 0 {
		return response(started, "system", map[string]any{}, nil, cosmos.NewError(cosmos.CodeInvalidInput, "at least one transaction event filter is required", nil))
	}
	for _, event := range in.Events {
		if strings.TrimSpace(event) == "" {
			return response(started, "system", map[string]any{}, nil, cosmos.NewError(cosmos.CodeInvalidInput, "transaction event filters must not be empty", nil))
		}
	}
	order := strings.ToLower(strings.TrimSpace(in.Order))
	if order == "" {
		order = "desc"
	}
	if order != "asc" && order != "desc" {
		return response(started, "system", map[string]any{}, nil, cosmos.NewError(cosmos.CodeInvalidInput, "order must be asc or desc", nil))
	}
	page, grpcPage, lcdPage, err := paginationValues(in.Pagination)
	if err != nil {
		return response(started, "system", map[string]any{}, nil, err)
	}
	if page.Key != "" {
		return response(started, "system", map[string]any{}, nil, cosmos.NewError(cosmos.CodeInvalidInput, "pagination key is not supported for transaction search", nil))
	}
	if page.Reverse {
		return response(started, "system", map[string]any{}, nil, cosmos.NewError(cosmos.CodeInvalidInput, "pagination reverse is not supported for transaction search; use order instead", nil))
	}
	if page.Offset%page.Limit != 0 {
		return response(started, "system", map[string]any{}, nil, cosmos.NewError(cosmos.CodeInvalidInput, "transaction search offset must fall on a page boundary", nil))
	}
	pageNumber := page.Offset/page.Limit + 1
	eventQuery := strings.Join(in.Events, " AND ")
	orderBy := "ORDER_BY_" + strings.ToUpper(order)
	grpcRequest := map[string]any{
		"events": in.Events, "pagination": grpcPage, "order_by": orderBy,
		"query": eventQuery, "page": strconv.FormatUint(pageNumber, 10), "limit": strconv.FormatUint(page.Limit, 10),
	}
	lcdMulti := make(map[string][]string, len(lcdPage)+5)
	for key, value := range lcdPage {
		lcdMulti[key] = []string{value}
	}
	lcdMulti["events"] = append([]string(nil), in.Events...)
	lcdMulti["query"] = []string{eventQuery}
	lcdMulti["page"] = []string{strconv.FormatUint(pageNumber, 10)}
	lcdMulti["limit"] = []string{strconv.FormatUint(page.Limit, 10)}
	lcdMulti["order_by"] = []string{orderBy}
	bindings := []cosmos.Binding{
		{Name: "grpc:tx_search", GRPCMethod: "/cosmos.tx.v1beta1.Service/GetTxsEvent", GRPCRequest: grpcRequest, GRPCDiscardUnknown: true},
		{Name: "lcd:tx_search", LCDPath: "/cosmos/tx/v1beta1/txs", LCDQueryMulti: lcdMulti},
		{Name: "rpc:tx_search", RPCMethod: "tx_search", RPCParams: map[string]any{
			"query": eventQuery, "order_by": order, "page": strconv.FormatUint(pageNumber, 10),
			"per_page": strconv.FormatUint(page.Limit, 10), "prove": false,
		}},
	}
	resolved, err := s.query.Resolve(ctx, "transaction_search", bindings)
	if err == nil {
		resolved.Data = normalizeTransactionSearch(resolved.Data)
	}
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{"events": in.Events, "order": order, "pagination": in.Pagination}, resolved.Data, err)
}

func (s *Server) simulateTransaction(ctx context.Context, _ *mcp.CallToolRequest, in simulationInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	if in.TxBytes == "" {
		return response(started, "system", map[string]any{}, nil, cosmos.NewError(cosmos.CodeInvalidInput, "tx_bytes must not be empty", nil))
	}
	decoded, err := base64.StdEncoding.DecodeString(in.TxBytes)
	if err != nil || len(decoded) == 0 {
		return response(started, "system", map[string]any{}, nil, cosmos.NewError(cosmos.CodeInvalidInput, "tx_bytes must be non-empty base64", err))
	}
	resolved, err := s.query.Resolve(ctx, "transaction_simulation", []cosmos.Binding{{Name: "grpc:simulate", GRPCMethod: "/cosmos.tx.v1beta1.Service/Simulate", GRPCRequest: map[string]any{"tx_bytes": in.TxBytes}}})
	if err == nil {
		resolved.Data = normalizeFields(resolved.Data, "gas_info", "result")
	}
	return responseBound(started, resolved.Source, resolved.Binding, map[string]any{"tx_bytes_length": len(decoded)}, resolved.Data, err)
}

type pageValues struct {
	Key                 string
	Offset, Limit       uint64
	CountTotal, Reverse bool
}

func normalizedPagination(input *paginationInput) pageValues {
	result := pageValues{Limit: defaultPageLimit}
	if input != nil {
		result = pageValues{Key: input.Key, Offset: input.Offset, Limit: input.Limit, CountTotal: input.CountTotal, Reverse: input.Reverse}
		if result.Limit == 0 {
			result.Limit = defaultPageLimit
		}
	}
	return result
}

func pagination(input *paginationInput) (map[string]any, map[string]string, error) {
	_, grpc, lcd, err := paginationValues(input)
	return grpc, lcd, err
}

// paginationValues normalizes pagination once and returns it alongside the
// derived gRPC and LCD representations, so callers that also need the
// normalized values (such as searchTransactions) don't normalize twice.
func paginationValues(input *paginationInput) (pageValues, map[string]any, map[string]string, error) {
	p := normalizedPagination(input)
	if p.Limit > maxPageLimit {
		return pageValues{}, nil, nil, cosmos.NewError(cosmos.CodeInvalidInput, "pagination limit cannot exceed 200", nil)
	}
	if p.Key != "" && p.Offset != 0 {
		return pageValues{}, nil, nil, cosmos.NewError(cosmos.CodeInvalidInput, "pagination key and offset cannot be used together", nil)
	}
	grpc := map[string]any{"limit": strconv.FormatUint(p.Limit, 10), "count_total": p.CountTotal, "reverse": p.Reverse}
	lcd := map[string]string{"pagination.limit": strconv.FormatUint(p.Limit, 10), "pagination.count_total": strconv.FormatBool(p.CountTotal), "pagination.reverse": strconv.FormatBool(p.Reverse)}
	if p.Key != "" {
		grpc["key"] = p.Key
		lcd["pagination.key"] = p.Key
	}
	if p.Offset != 0 {
		grpc["offset"] = strconv.FormatUint(p.Offset, 10)
		lcd["pagination.offset"] = strconv.FormatUint(p.Offset, 10)
	}
	return p, grpc, lcd, nil
}

func normalizeFields(raw any, keys ...string) map[string]any {
	result := map[string]any{"raw": raw}
	value := object(raw)
	for _, key := range keys {
		if item, ok := value[key]; ok {
			result[key] = item
		}
	}
	return result
}

func normalizeList(raw any, key string) map[string]any {
	result := normalizeFields(raw, key)
	page := object(object(raw)["pagination"])
	result["pagination"] = map[string]any{"next_key": page["next_key"], "total": page["total"]}
	return result
}

func normalizeTransactionSearch(raw any) map[string]any {
	result := normalizeFields(raw, "txs", "tx_responses", "total_count")
	if _, ok := result["tx_responses"]; !ok {
		if txs, exists := object(raw)["txs"]; exists {
			result["tx_responses"] = txs
		}
	}
	page := object(object(raw)["pagination"])
	result["pagination"] = map[string]any{"next_key": page["next_key"], "total": firstNonNil(page["total"], object(raw)["total_count"], object(raw)["total"])}
	return result
}

func normalizeNodeStatus(nodeRaw, syncRaw any) map[string]any {
	node := object(nodeRaw)
	defaultNode := object(node["default_node_info"])
	application := object(node["application_version"])
	if len(defaultNode) == 0 {
		defaultNode = object(node["node_info"])
	}
	syncing := object(syncRaw)
	return map[string]any{
		"chain_id":     firstNonNil(defaultNode["network"], node["chain_id"]),
		"node_version": firstNonNil(application["version"], defaultNode["version"]),
		"catching_up":  firstNonNil(syncing["syncing"], syncing["catching_up"]),
		"node_info":    firstNonNil(node["default_node_info"], node["node_info"]),
	}
}

func withRaw(value any, raw any) map[string]any {
	result := object(value)
	dup := make(map[string]any, len(result)+1)
	maps.Copy(dup, result)
	dup["raw"] = raw
	return dup
}

func object(value any) map[string]any { result, _ := value.(map[string]any); return result }
func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}
func boolKey(value bool) string {
	if value {
		return "height"
	}
	return "latest"
}

func safeValue(raw, name string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.ContainsAny(value, "/?# \\") {
		return "", cosmos.NewError(cosmos.CodeInvalidInput, name+" must be a non-empty safe value", nil)
	}
	return value, nil
}

func safeDenom(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" || strings.ContainsAny(value, "?# \\") {
		return "", cosmos.NewError(cosmos.CodeInvalidInput, "denom must be a non-empty safe value", nil)
	}
	return value, nil
}

func normalizedHash(raw string) (string, error) {
	value := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(raw), "0x"), "0X")
	if value == "" || len(value)%2 != 0 {
		return "", cosmos.NewError(cosmos.CodeInvalidInput, "transaction hash must be a non-empty hexadecimal string", nil)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", cosmos.NewError(cosmos.CodeInvalidInput, "transaction hash must be hexadecimal", err)
	}
	return strings.ToUpper(value), nil
}

func commonSource(values []string) string {
	if len(values) == 0 {
		return "system"
	}
	for _, value := range values[1:] {
		if value != values[0] {
			return "mixed"
		}
	}
	return values[0]
}

func optionalCapabilityError(err error) bool {
	code, _ := cosmos.ErrorDetails(err)
	return code == cosmos.CodeUnsupportedCapability || code == cosmos.CodeGRPCReflectionUnavailable
}
