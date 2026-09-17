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
	ibcDenomPrefix   = "ibc/"
)

func validProposalID(raw string) error {
	if value, err := strconv.ParseUint(raw, 10, 64); err != nil || value == 0 {
		return cosmos.NewError(cosmos.CodeInvalidInput, "proposal_id must be a positive decimal string", err)
	}
	return nil
}

// denomTraceBindings resolves an IBC voucher hash to its origin chain and path.
// ibc-go v9 renamed DenomTrace to Denom and /denom_traces to /denoms; the older
// names are tried first because they are what the large majority of live chains
// still serve.
func denomTraceBindings(hash string) []cosmos.Binding {
	return []cosmos.Binding{
		{Name: "grpc:denom_trace", GRPCMethod: "/ibc.applications.transfer.v1.Query/DenomTrace", GRPCRequest: map[string]any{"hash": hash}},
		{Name: "lcd:denom_trace", LCDPath: "/ibc/apps/transfer/v1/denom_traces/" + url.PathEscape(hash)},
		{Name: "grpc:denom", GRPCMethod: "/ibc.applications.transfer.v1.Query/Denom", GRPCRequest: map[string]any{"hash": hash}},
		{Name: "lcd:denom", LCDPath: "/ibc/apps/transfer/v1/denoms/" + url.PathEscape(hash)},
	}
}

// heightContext validates an optional historical block height and pins every
// LCD and gRPC read made with the returned context to it.
func heightContext(ctx context.Context, height string) (context.Context, error) {
	if height == "" {
		return ctx, nil
	}
	if value, err := strconv.ParseUint(height, 10, 64); err != nil || value == 0 {
		return nil, cosmos.NewError(cosmos.CodeInvalidInput, "height must be a positive decimal string", err)
	}
	return cosmos.WithHeight(ctx, height), nil
}

// simpleQuery runs the height -> resolve -> normalize -> respond path shared by
// the module queries. A non-empty listKey selects list normalization, which
// also surfaces the continuation key; otherwise keys are lifted as plain fields.
func (s *Server) simpleQuery(ctx context.Context, started time.Time, capability, height string, request map[string]any, bindings []cosmos.Binding, listKey string, keys ...string) (*mcp.CallToolResult, ToolResponse, error) {
	ctx, err := heightContext(ctx, height)
	if err != nil {
		return response(started, "system", request, nil, err)
	}
	resolved, err := s.query.Resolve(ctx, capability, bindings)
	if err == nil {
		if listKey != "" {
			resolved.Data = normalizeList(resolved.Data, listKey)
		} else {
			resolved.Data = normalizeFields(resolved.Data, keys...)
		}
	}
	return responseBound(started, resolved.Source, resolved.Binding, request, resolved.Data, err)
}

// govBindings pairs the v1 and v1beta1 routes for one x/gov query. v1 replaced
// v1beta1 in SDK v0.46 but many live chains still serve only the older API, and
// nothing in a response reliably reports which one the chain has.
func govBindings(name, method, path string, request map[string]any, query map[string]string) []cosmos.Binding {
	return []cosmos.Binding{
		{Name: "grpc:gov_v1_" + name, GRPCMethod: "/cosmos.gov.v1.Query/" + method, GRPCRequest: request, GRPCDiscardUnknown: true},
		{Name: "grpc:gov_v1beta1_" + name, GRPCMethod: "/cosmos.gov.v1beta1.Query/" + method, GRPCRequest: request, GRPCDiscardUnknown: true},
		{Name: "lcd:gov_v1_" + name, LCDPath: "/cosmos/gov/v1" + path, LCDQuery: query},
		{Name: "lcd:gov_v1beta1_" + name, LCDPath: "/cosmos/gov/v1beta1" + path, LCDQuery: query},
	}
}

// moduleBindings pairs the gRPC and LCD routes for a query that exists at the
// same name in exactly one version of a module's Query service. Binding names
// are derived from the method rather than the path so that they stay stable
// across calls when the path embeds an address, which is what lets the resolver
// reuse a cached binding.
func moduleBindings(module, method, path string, request map[string]any, query map[string]string) []cosmos.Binding {
	name := module + "_" + strings.ToLower(method)
	return []cosmos.Binding{
		{Name: "grpc:" + name, GRPCMethod: "/cosmos." + module + ".v1beta1.Query/" + method, GRPCRequest: request},
		{Name: "lcd:" + name, LCDPath: "/cosmos/" + module + "/v1beta1/" + path, LCDQuery: query},
	}
}

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
	return s.simpleQuery(ctx, started, "account", in.Height, map[string]any{"address": address, "height": in.Height}, []cosmos.Binding{
		{Name: "grpc:auth_account", GRPCMethod: "/cosmos.auth.v1beta1.Query/Account", GRPCRequest: map[string]any{"address": address}},
		{Name: "lcd:auth_account", LCDPath: "/cosmos/auth/v1beta1/accounts/" + url.PathEscape(address)},
	}, "", "account")
}

func (s *Server) resolveBalances(ctx context.Context, in balancesInput) (cosmos.Resolution, error) {
	address, err := safeValue(in.Address, "address")
	if err != nil {
		return cosmos.Resolution{}, err
	}
	ctx, err = heightContext(ctx, in.Height)
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
	ctx, err = heightContext(ctx, in.Height)
	if err != nil {
		return response(started, "system", map[string]any{"denom": in.Denom, "height": in.Height}, nil, err)
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
	// An ibc/HASH denom says nothing about what the token actually is, so
	// resolve the trace alongside it. Failure is tolerated the same way a
	// missing supply or metadata query is.
	if hash, isIBC := strings.CutPrefix(denom, ibcDenomPrefix); isIBC {
		trace, traceErr := s.query.Resolve(ctx, "denom_trace", denomTraceBindings(hash))
		switch {
		case traceErr == nil:
			data["denom_trace"] = firstNonNil(object(trace.Data)["denom_trace"], object(trace.Data)["denom"])
			data["raw"].(map[string]any)["denom_trace"] = trace.Data
			sources, bindings = append(sources, trace.Source), append(bindings, trace.Binding)
		case !optionalCapabilityError(traceErr):
			return responseBound(started, trace.Source, trace.Binding, map[string]any{"denom": denom}, nil, traceErr)
		default:
			warnings = append(warnings, "IBC denomination trace is unavailable")
		}
	}
	if len(warnings) > 0 {
		data["warnings"] = warnings
	}
	if len(sources) == 0 {
		return response(started, "system", map[string]any{"denom": denom}, nil, supplyErr)
	}
	return responseBound(started, commonSource(sources), strings.Join(bindings, ";"), map[string]any{"denom": denom, "height": in.Height}, data, nil)
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
	return s.simpleQuery(ctx, started, "staking_validators", in.Height, map[string]any{"status": in.Status, "pagination": in.Pagination, "height": in.Height}, []cosmos.Binding{
		{Name: "grpc:staking_validators", GRPCMethod: "/cosmos.staking.v1beta1.Query/Validators", GRPCRequest: request},
		{Name: "lcd:staking_validators", LCDPath: "/cosmos/staking/v1beta1/validators", LCDQuery: lcdPage},
	}, "validators")
}

func (s *Server) getValidator(ctx context.Context, _ *mcp.CallToolRequest, in validatorInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	address, err := safeValue(in.ValidatorAddress, "validator_address")
	if err != nil {
		return response(started, "system", map[string]any{}, nil, err)
	}
	return s.simpleQuery(ctx, started, "staking_validator", in.Height, map[string]any{"validator_address": address, "height": in.Height}, []cosmos.Binding{
		{Name: "grpc:staking_validator", GRPCMethod: "/cosmos.staking.v1beta1.Query/Validator", GRPCRequest: map[string]any{"validator_addr": address}},
		{Name: "lcd:staking_validator", LCDPath: "/cosmos/staking/v1beta1/validators/" + url.PathEscape(address)},
	}, "", "validator")
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
	return s.simpleQuery(ctx, started, capability, in.Height, map[string]any{"delegator_address": address, "pagination": in.Pagination, "height": in.Height}, []cosmos.Binding{
		{Name: "grpc:" + capability, GRPCMethod: "/cosmos.staking.v1beta1.Query/" + method, GRPCRequest: map[string]any{"delegator_addr": address, "pagination": grpcPage}},
		{Name: "lcd:" + capability, LCDPath: path, LCDQuery: lcdPage},
	}, key)
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
	echo := map[string]any{"height": in.Height}
	maps.Copy(echo, request)
	return s.simpleQuery(ctx, started, capability, in.Height, echo, []cosmos.Binding{
		{Name: "grpc:" + capability, GRPCMethod: "/cosmos.distribution.v1beta1.Query/" + method, GRPCRequest: request},
		{Name: "lcd:" + capability, LCDPath: path},
	}, "", keys...)
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
	return s.simpleQuery(ctx, started, "governance_proposals", in.Height,
		map[string]any{"status": in.Status, "voter": in.Voter, "depositor": in.Depositor, "pagination": in.Pagination, "height": in.Height},
		govBindings("proposals", "Proposals", "/proposals", request, lcdPage), "proposals")
}

func (s *Server) getProposal(ctx context.Context, _ *mcp.CallToolRequest, in proposalInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	if err := validProposalID(in.ProposalID); err != nil {
		return response(started, "system", map[string]any{"proposal_id": in.ProposalID}, nil, err)
	}
	request := map[string]any{"proposal_id": in.ProposalID}
	return s.simpleQuery(ctx, started, "governance_proposal", in.Height,
		map[string]any{"proposal_id": in.ProposalID, "height": in.Height},
		govBindings("proposal", "Proposal", "/proposals/"+in.ProposalID, request, nil), "", "proposal")
}

func (s *Server) getProposalTally(ctx context.Context, _ *mcp.CallToolRequest, in proposalInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	if err := validProposalID(in.ProposalID); err != nil {
		return response(started, "system", map[string]any{"proposal_id": in.ProposalID}, nil, err)
	}
	request := map[string]any{"proposal_id": in.ProposalID}
	return s.simpleQuery(ctx, started, "governance_tally", in.Height,
		map[string]any{"proposal_id": in.ProposalID, "height": in.Height},
		govBindings("tally", "TallyResult", "/proposals/"+in.ProposalID+"/tally", request, nil), "", "tally")
}

func (s *Server) getProposalVotes(ctx context.Context, _ *mcp.CallToolRequest, in proposalVotesInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	if err := validProposalID(in.ProposalID); err != nil {
		return response(started, "system", map[string]any{"proposal_id": in.ProposalID}, nil, err)
	}
	echo := map[string]any{"proposal_id": in.ProposalID, "voter": in.Voter, "pagination": in.Pagination, "height": in.Height}
	if in.Voter != "" {
		voter, err := safeValue(in.Voter, "voter")
		if err != nil {
			return response(started, "system", echo, nil, err)
		}
		return s.simpleQuery(ctx, started, "governance_vote", in.Height, echo,
			govBindings("vote", "Vote", "/proposals/"+in.ProposalID+"/votes/"+url.PathEscape(voter),
				map[string]any{"proposal_id": in.ProposalID, "voter": voter}, nil), "", "vote")
	}
	grpcPage, lcdPage, err := pagination(in.Pagination)
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	return s.simpleQuery(ctx, started, "governance_votes", in.Height, echo,
		govBindings("votes", "Votes", "/proposals/"+in.ProposalID+"/votes",
			map[string]any{"proposal_id": in.ProposalID, "pagination": grpcPage}, lcdPage), "votes")
}

func (s *Server) getProposalDeposits(ctx context.Context, _ *mcp.CallToolRequest, in proposalDepositsInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	if err := validProposalID(in.ProposalID); err != nil {
		return response(started, "system", map[string]any{"proposal_id": in.ProposalID}, nil, err)
	}
	echo := map[string]any{"proposal_id": in.ProposalID, "depositor": in.Depositor, "pagination": in.Pagination, "height": in.Height}
	if in.Depositor != "" {
		depositor, err := safeValue(in.Depositor, "depositor")
		if err != nil {
			return response(started, "system", echo, nil, err)
		}
		return s.simpleQuery(ctx, started, "governance_deposit", in.Height, echo,
			govBindings("deposit", "Deposit", "/proposals/"+in.ProposalID+"/deposits/"+url.PathEscape(depositor),
				map[string]any{"proposal_id": in.ProposalID, "depositor": depositor}, nil), "", "deposit")
	}
	grpcPage, lcdPage, err := pagination(in.Pagination)
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	return s.simpleQuery(ctx, started, "governance_deposits", in.Height, echo,
		govBindings("deposits", "Deposits", "/proposals/"+in.ProposalID+"/deposits",
			map[string]any{"proposal_id": in.ProposalID, "pagination": grpcPage}, lcdPage), "deposits")
}

// govParamsBindings builds the routes for one x/gov parameter group. Unlike
// every other module, x/gov keys its parameters by type, so the type is a
// required path segment rather than an optional filter.
func govParamsBindings(paramsType string) []cosmos.Binding {
	return govBindings("params_"+paramsType, "Params", "/params/"+paramsType,
		map[string]any{"params_type": paramsType}, nil)
}

// getGovParams reads x/gov parameters. gov v1 returns the whole set in "params"
// whatever params_type is asked for, so one query is enough there. v1beta1 has
// no "params" field at all and populates only the group named, so reading it
// completely takes one query per group - asking a v1beta1 chain for "voting"
// alone yields the voting period and nothing else, silently omitting the quorum
// and deposit parameters this tool advertises.
func (s *Server) getGovParams(ctx context.Context, _ *mcp.CallToolRequest, in heightInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	echo := map[string]any{"height": in.Height}
	ctx, err := heightContext(ctx, in.Height)
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	voting, err := s.query.Resolve(ctx, "governance_params", govParamsBindings("voting"))
	if err != nil {
		return responseBound(started, voting.Source, voting.Binding, echo, nil, err)
	}
	data := normalizeFields(voting.Data, "params", "voting_params", "deposit_params", "tally_params")
	if object(voting.Data)["params"] != nil {
		return responseBound(started, voting.Source, voting.Binding, echo, data, nil)
	}

	raw := map[string]any{"voting": voting.Data}
	warnings := []string{}
	sources, bindings := []string{voting.Source}, []string{voting.Binding}
	for _, group := range []struct{ paramsType, key string }{
		{"deposit", "deposit_params"},
		{"tallying", "tally_params"},
	} {
		resolved, groupErr := s.query.Resolve(ctx, "governance_params_"+group.paramsType, govParamsBindings(group.paramsType))
		if groupErr != nil {
			if !optionalCapabilityError(groupErr) {
				return responseBound(started, resolved.Source, resolved.Binding, echo, nil, groupErr)
			}
			warnings = append(warnings, group.key+" are unavailable")
			continue
		}
		data[group.key] = object(resolved.Data)[group.key]
		raw[group.paramsType] = resolved.Data
		sources, bindings = append(sources, resolved.Source), append(bindings, resolved.Binding)
	}
	data["raw"] = raw
	if len(warnings) > 0 {
		data["warnings"] = warnings
	}
	return responseBound(started, commonSource(sources), strings.Join(bindings, ";"), echo, data, nil)
}

func (s *Server) getRedelegations(ctx context.Context, _ *mcp.CallToolRequest, in redelegationsInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	echo := map[string]any{"delegator_address": in.DelegatorAddress, "src_validator_address": in.SrcValidatorAddress, "dst_validator_address": in.DstValidatorAddress, "pagination": in.Pagination, "height": in.Height}
	delegator, err := safeValue(in.DelegatorAddress, "delegator_address")
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	grpcPage, lcdPage, err := pagination(in.Pagination)
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	request := map[string]any{"delegator_addr": delegator, "pagination": grpcPage}
	for _, filter := range []struct{ name, raw string }{
		{"src_validator_addr", in.SrcValidatorAddress},
		{"dst_validator_addr", in.DstValidatorAddress},
	} {
		if filter.raw == "" {
			continue
		}
		value, valueErr := safeValue(filter.raw, filter.name)
		if valueErr != nil {
			return response(started, "system", echo, nil, valueErr)
		}
		request[filter.name] = value
		lcdPage[filter.name] = value
	}
	return s.simpleQuery(ctx, started, "redelegations", in.Height, echo, []cosmos.Binding{
		{Name: "grpc:redelegations", GRPCMethod: "/cosmos.staking.v1beta1.Query/Redelegations", GRPCRequest: request},
		{Name: "lcd:redelegations", LCDPath: "/cosmos/staking/v1beta1/delegators/" + url.PathEscape(delegator) + "/redelegations", LCDQuery: lcdPage},
	}, "redelegation_responses")
}

func (s *Server) getValidatorDelegations(ctx context.Context, _ *mcp.CallToolRequest, in validatorDelegationsInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	echo := map[string]any{"validator_address": in.ValidatorAddress, "pagination": in.Pagination, "height": in.Height}
	validator, err := safeValue(in.ValidatorAddress, "validator_address")
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	grpcPage, lcdPage, err := pagination(in.Pagination)
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	return s.simpleQuery(ctx, started, "validator_delegations", in.Height, echo, []cosmos.Binding{
		{Name: "grpc:validator_delegations", GRPCMethod: "/cosmos.staking.v1beta1.Query/ValidatorDelegations", GRPCRequest: map[string]any{"validator_addr": validator, "pagination": grpcPage}},
		{Name: "lcd:validator_delegations", LCDPath: "/cosmos/staking/v1beta1/validators/" + url.PathEscape(validator) + "/delegations", LCDQuery: lcdPage},
	}, "delegation_responses")
}

func (s *Server) getStakingPool(ctx context.Context, _ *mcp.CallToolRequest, in heightInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	return s.simpleQuery(ctx, started, "staking_pool", in.Height, map[string]any{"height": in.Height},
		moduleBindings("staking", "Pool", "pool", map[string]any{}, nil), "", "pool")
}

func (s *Server) getCommunityPool(ctx context.Context, _ *mcp.CallToolRequest, in heightInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	return s.simpleQuery(ctx, started, "community_pool", in.Height, map[string]any{"height": in.Height},
		moduleBindings("distribution", "CommunityPool", "community_pool", map[string]any{}, nil), "", "pool")
}

func (s *Server) getTotalSupply(ctx context.Context, _ *mcp.CallToolRequest, in pagedInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	echo := map[string]any{"pagination": in.Pagination, "height": in.Height}
	grpcPage, lcdPage, err := pagination(in.Pagination)
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	return s.simpleQuery(ctx, started, "total_supply", in.Height, echo,
		moduleBindings("bank", "TotalSupply", "supply", map[string]any{"pagination": grpcPage}, lcdPage), "supply")
}

func (s *Server) getSigningInfos(ctx context.Context, _ *mcp.CallToolRequest, in signingInfosInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	echo := map[string]any{"consensus_address": in.ConsensusAddress, "pagination": in.Pagination, "height": in.Height}
	if in.ConsensusAddress != "" {
		address, err := safeValue(in.ConsensusAddress, "consensus_address")
		if err != nil {
			return response(started, "system", echo, nil, err)
		}
		return s.simpleQuery(ctx, started, "signing_info", in.Height, echo,
			moduleBindings("slashing", "SigningInfo", "signing_infos/"+url.PathEscape(address), map[string]any{"cons_address": address}, nil),
			"", "val_signing_info")
	}
	grpcPage, lcdPage, err := pagination(in.Pagination)
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	return s.simpleQuery(ctx, started, "signing_infos", in.Height, echo,
		moduleBindings("slashing", "SigningInfos", "signing_infos", map[string]any{"pagination": grpcPage}, lcdPage), "info")
}

func (s *Server) getDenomTrace(ctx context.Context, _ *mcp.CallToolRequest, in denomTraceInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	echo := map[string]any{"denom": in.Denom, "height": in.Height}
	hash, err := safeValue(strings.TrimPrefix(strings.TrimSpace(in.Denom), ibcDenomPrefix), "denom")
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	return s.simpleQuery(ctx, started, "denom_trace", in.Height, echo, denomTraceBindings(hash), "", "denom_trace", "denom")
}

// getInflation reports the current inflation rate and annual provisions, which
// are separate x/mint queries; one can be present without the other, and chains
// that replaced the standard x/mint expose neither.
func (s *Server) getInflation(ctx context.Context, _ *mcp.CallToolRequest, in heightInput) (*mcp.CallToolResult, ToolResponse, error) {
	started := time.Now()
	echo := map[string]any{"height": in.Height}
	ctx, err := heightContext(ctx, in.Height)
	if err != nil {
		return response(started, "system", echo, nil, err)
	}
	inflation, inflationErr := s.query.Resolve(ctx, "mint_inflation", moduleBindings("mint", "Inflation", "inflation", map[string]any{}, nil))
	if inflationErr != nil && !optionalCapabilityError(inflationErr) {
		return responseBound(started, inflation.Source, inflation.Binding, echo, nil, inflationErr)
	}
	provisions, provisionsErr := s.query.Resolve(ctx, "mint_annual_provisions", moduleBindings("mint", "AnnualProvisions", "annual_provisions", map[string]any{}, nil))
	if provisionsErr != nil && !optionalCapabilityError(provisionsErr) {
		return responseBound(started, provisions.Source, provisions.Binding, echo, nil, provisionsErr)
	}
	if inflationErr != nil && provisionsErr != nil {
		return response(started, "system", echo, nil, inflationErr)
	}
	data := map[string]any{"raw": map[string]any{}}
	warnings := []string{}
	sources, bindings := []string{}, []string{}
	if inflationErr == nil {
		data["inflation"] = object(inflation.Data)["inflation"]
		data["raw"].(map[string]any)["inflation"] = inflation.Data
		sources, bindings = append(sources, inflation.Source), append(bindings, inflation.Binding)
	} else {
		warnings = append(warnings, "inflation is unavailable")
	}
	if provisionsErr == nil {
		data["annual_provisions"] = object(provisions.Data)["annual_provisions"]
		data["raw"].(map[string]any)["annual_provisions"] = provisions.Data
		sources, bindings = append(sources, provisions.Source), append(bindings, provisions.Binding)
	} else {
		warnings = append(warnings, "annual provisions are unavailable")
	}
	if len(warnings) > 0 {
		data["warnings"] = warnings
	}
	return responseBound(started, commonSource(sources), strings.Join(bindings, ";"), echo, data, nil)
}

// moduleParams builds a handler for a module whose parameters live at the
// conventional v1beta1 Params method and /params route. x/gov does not follow
// this shape and is handled by getGovParams.
func (s *Server) moduleParams(module string) func(context.Context, *mcp.CallToolRequest, heightInput) (*mcp.CallToolResult, ToolResponse, error) {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in heightInput) (*mcp.CallToolResult, ToolResponse, error) {
		started := time.Now()
		return s.simpleQuery(ctx, started, module+"_params", in.Height, map[string]any{"height": in.Height},
			moduleBindings(module, "Params", "params", map[string]any{}, nil), "", "params")
	}
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
