//  Copyright (c) 2019 Couchbase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// 		http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cbft

import (
	"fmt"
	"golang.org/x/net/context"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/blevesearch/bleve"
	"github.com/blevesearch/bleve/document"
	"github.com/blevesearch/bleve/index"
	"github.com/blevesearch/bleve/index/store"
	"github.com/blevesearch/bleve/mapping"
	"github.com/blevesearch/bleve/search"
	pb "github.com/couchbase/cbft/protobuf"
	"github.com/couchbase/cbgt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	log "github.com/couchbase/clog"
)

// GrpcClient implements the Search() and DocCount() subset of the
// bleve.Index interface by accessing a remote cbft server via grpc
// protocol.  This allows callers to add a GrpcClient as a target of
// a bleve.IndexAlias, and implements cbft protocol features like
// query consistency and auth.
type GrpcClient struct {
	Mgr         *cbgt.Manager
	name        string
	HostPort    string
	IndexName   string
	IndexUUID   string
	PIndexNames []string

	Consistency *cbgt.ConsistencyParams
	GrpcCli     pb.SearchServiceClient

	lastMutex        sync.RWMutex
	lastSearchStatus int
	lastErrBody      []byte
	sc               streamHandler
}

func (g *GrpcClient) SetStreamHandler(sc streamHandler) {
	g.sc = sc
}

func (g *GrpcClient) GetHostPort() string {
	return g.HostPort
}

func (g *GrpcClient) GetLast() (int, []byte) {
	g.lastMutex.RLock()
	defer g.lastMutex.RUnlock()
	return g.lastSearchStatus, g.lastErrBody
}

func (g *GrpcClient) Name() string {
	return g.name
}

func (g *GrpcClient) SetName(name string) {
	g.name = name
}

func (g *GrpcClient) Index(id string, data interface{}) error {
	return indexClientUnimplementedErr
}

func (g *GrpcClient) Delete(id string) error {
	return indexClientUnimplementedErr
}

func (g *GrpcClient) Batch(b *bleve.Batch) error {
	return indexClientUnimplementedErr
}

func (g *GrpcClient) Document(id string) (*document.Document, error) {
	return nil, indexClientUnimplementedErr
}

func (g *GrpcClient) DocCount() (uint64, error) {
	request := &pb.DocCountRequest{IndexName: g.PIndexNames[0],
		IndexUUID: ""}
	res, err := g.GrpcCli.DocCount(context.Background(), request)
	if err != nil {
		return 0, err
	}
	return uint64(res.DocCount), nil
}

func (g *GrpcClient) Search(req *bleve.SearchRequest) (
	*bleve.SearchResult, error) {
	return g.SearchInContext(context.Background(), req)
}

func (g *GrpcClient) SearchInContext(ctx context.Context,
	req *bleve.SearchRequest) (*bleve.SearchResult, error) {
	if req == nil {
		return nil, fmt.Errorf("grpc_client: SearchInContext, no req provided")
	}

	queryCtlParams := &cbgt.QueryCtlParams{
		Ctl: cbgt.QueryCtl{
			Consistency: g.Consistency,
		},
	}

	queryPIndexes := &QueryPIndexes{
		PIndexNames: g.PIndexNames,
	}

	// if timeout was set, compute time remaining
	if deadline, ok := ctx.Deadline(); ok {
		remaining := deadline.Sub(time.Now())
		// FIXME arbitrarily reducing the timeout, to increase the liklihood
		// that a live system replies via HTTP round-trip before we give up
		// on the request externally
		remaining -= RemoteRequestOverhead
		if remaining < 0 {
			// not enough time left
			return nil, context.DeadlineExceeded
		}
		queryCtlParams.Ctl.Timeout = int64(remaining / time.Millisecond)
	}

	sr := &scatterRequest{
		ctlParams:     queryCtlParams,
		onlyPIndexes:  queryPIndexes,
		searchRequest: req,
	}

	resultCh := make(chan *bleve.SearchResult)

	go func() {
		rv, err := g.Query(ctx, sr)
		if err != nil {
			resultCh <- makeSearchResultErr(req, g.PIndexNames, err)
			return
		}

		resultCh <- rv
	}()

	select {
	case <-ctx.Done():
		return makeSearchResultErr(req, g.PIndexNames, ctx.Err()), nil
	case rv := <-resultCh:
		return rv, nil
	}
}

type scatterRequest struct {
	ctlParams     *cbgt.QueryCtlParams
	onlyPIndexes  *QueryPIndexes
	searchRequest *bleve.SearchRequest
}

func (g *GrpcClient) Fields() ([]string, error) {
	return nil, indexClientUnimplementedErr
}

func (g *GrpcClient) FieldDict(field string) (index.FieldDict, error) {
	return nil, indexClientUnimplementedErr
}

func (g *GrpcClient) FieldDictRange(field string,
	startTerm []byte, endTerm []byte) (index.FieldDict, error) {
	return nil, indexClientUnimplementedErr
}

func (g *GrpcClient) FieldDictPrefix(field string,
	termPrefix []byte) (index.FieldDict, error) {
	return nil, indexClientUnimplementedErr
}

func (g *GrpcClient) DumpAll() chan interface{} {
	return nil
}

func (g *GrpcClient) DumpDoc(id string) chan interface{} {
	return nil
}

func (g *GrpcClient) DumpFields() chan interface{} {
	return nil
}

func (g *GrpcClient) Close() error {
	return indexClientUnimplementedErr
}

func (g *GrpcClient) Mapping() mapping.IndexMapping {
	return nil
}

func (g *GrpcClient) NewBatch() *bleve.Batch {
	return nil
}

func (g *GrpcClient) Stats() *bleve.IndexStat {
	return nil
}

func (g *GrpcClient) StatsMap() map[string]interface{} {
	return nil
}

func (g *GrpcClient) GetInternal(key []byte) ([]byte, error) {
	return nil, indexClientUnimplementedErr
}

func (g *GrpcClient) SetInternal(key, val []byte) error {
	return indexClientUnimplementedErr
}

func (g *GrpcClient) DeleteInternal(key []byte) error {
	return indexClientUnimplementedErr
}

func (g *GrpcClient) Count() (uint64, error) {
	return g.DocCount()
}

func (g *GrpcClient) SearchRPC(ctx context.Context, req *scatterRequest,
	pbReq *pb.SearchRequest) (*bleve.SearchResult, error) {
	res, err := g.GrpcCli.Search(ctx, pbReq)
	if err != nil || res == nil {
		log.Printf("grpc_client: search err: %v", err)
		return nil, err
	}

	searchResult := &bleve.SearchResult{
		Status:  &bleve.SearchStatus{},
		Request: req.searchRequest,
	}

	var response *pb.StreamSearchResults
	for {
		response, err = res.Recv()
		if err == io.EOF {
			err = nil
			break
		}
		if err != nil {
			log.Printf("grpc_client: recv err: %v", err)
			break
		}

		switch r := response.Contents.(type) {

		case *pb.StreamSearchResults_Hits:
			if sw, ok := g.sc.(streamHandler); ok {
				err = sw.write(r.Hits.Bytes, r.Hits.Offsets, int(r.Hits.Total))
				if err != nil {
					break
				}
			}

		case *pb.StreamSearchResults_SearchResult:
			if r.SearchResult != nil {
				err = UnmarshalJSON(r.SearchResult, &searchResult)
				if err != nil {
					return searchResult, err
				}
			}
		}
	}

	return searchResult, err
}

func (g *GrpcClient) Query(ctx context.Context,
	req *scatterRequest) (*bleve.SearchResult, error) {
	scatterGatherReq := &pb.SearchRequest{
		IndexName: g.IndexName,
		IndexUUID: g.IndexUUID,
	}

	b, err := MarshalJSON(req.searchRequest)
	if err != nil {
		return nil, err
	}
	scatterGatherReq.Contents = b

	b, err = MarshalJSON(req.ctlParams)
	if err != nil {
		return nil, err
	}
	scatterGatherReq.QueryCtlParams = b

	b, err = MarshalJSON(req.onlyPIndexes)
	if err != nil {
		return nil, err
	}
	scatterGatherReq.QueryPIndexes = b

	// check if stream rpc is requested
	if se := ctx.Value(search.MakeDocumentMatchHandlerKey); se != nil {
		if _, ok := se.(search.MakeDocumentMatchHandler); ok {
			scatterGatherReq.Stream = true
		}
	}

	// mark that its a scatter gather query
	nctx := metadata.AppendToOutgoingContext(ctx,
		"rpcclusteractionkey", "fts/scatter-gather")

	return g.SearchRPC(nctx, req, scatterGatherReq)
}

func (g *GrpcClient) Advanced() (index.Index, store.KVStore, error) {
	return nil, nil, indexClientUnimplementedErr
}

// -----------------------------------------------------

func (g *GrpcClient) AuthType() string {
	if g.Mgr != nil {
		return g.Mgr.Options()["authType"]
	}
	return ""
}

func clientInterceptor(ctx context.Context, method string,
	req interface{}, reply interface{},
	cc *grpc.ClientConn, invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption) error {
	start := time.Now()
	err := invoker(ctx, method, req, reply, cc, opts...)
	log.Printf("grpc_client: invoke rpc method: %s duration: %f sec"+
		" err: %v", method, time.Since(start).Seconds(), err)
	return err
}

func addClientInterceptor() grpc.DialOption {
	return grpc.WithUnaryInterceptor(clientInterceptor)
}

func addGrpcClients(mgr *cbgt.Manager, indexName, indexUUID string,
	remotePlanPIndexes []*cbgt.RemotePlanPIndex,
	consistencyParams *cbgt.ConsistencyParams, onlyPIndexes map[string]bool,
	collector BleveIndexCollector, groupByNode bool) ([]RemoteClient, error) {
	remoteClients := make([]*GrpcClient, 0, len(remotePlanPIndexes))
	rv := make([]RemoteClient, 0, len(remotePlanPIndexes))

	for _, remotePlanPIndex := range remotePlanPIndexes {
		if onlyPIndexes != nil &&
			!onlyPIndexes[remotePlanPIndex.PlanPIndex.Name] {
			continue
		}

		delimiterPos := strings.LastIndex(remotePlanPIndex.NodeDef.HostPort, ":")
		if delimiterPos < 0 ||
			delimiterPos >= len(remotePlanPIndex.NodeDef.HostPort)-1 {
			// No port available
			log.Warnf("grpc_client: grpcClient with no possible port into: %v",
				remotePlanPIndex.NodeDef.HostPort)
			continue
		}
		host := remotePlanPIndex.NodeDef.HostPort[:delimiterPos]

		var port string
		bindPort, err := getPortFromNodeDefs(remotePlanPIndex.NodeDef, "bindGRPC")
		if err == nil {
			port = bindPort
		}

		ss := GetSecuritySetting()
		var certInBytes []byte
		if ss.EncryptionEnabled {
			bindPort, err = getPortFromNodeDefs(remotePlanPIndex.NodeDef, "bindGRPCSSL")
			if err == nil {
				port = bindPort
				certInBytes = ss.CertInBytes
			}
		}

		if port == "" {
			return nil, fmt.Errorf("grpc_client: no ports found for host: %s", host)
		}

		host = host + ":" + port

		cli, err := getRpcClient(remotePlanPIndex.NodeDef.UUID, host, certInBytes)
		if err != nil {
			log.Printf("grpc_client, getRpcClient err: %v", err)
			continue
		}

		grpcClient := &GrpcClient{
			Mgr:         mgr,
			name:        fmt.Sprintf("grpcClient - %s", host),
			HostPort:    host,
			IndexName:   indexName,
			IndexUUID:   indexUUID,
			PIndexNames: []string{remotePlanPIndex.PlanPIndex.Name},
			Consistency: consistencyParams,
			GrpcCli:     cli,
		}

		remoteClients = append(remoteClients, grpcClient)
	}

	if groupByNode {
		remoteClients = GroupGrpcClientsByHostPort(remoteClients)
	}

	for _, remoteClient := range remoteClients {
		collector.Add(remoteClient)
		rv = append(rv, remoteClient)
	}

	return rv, nil
}

func getPortFromNodeDefs(nodeDef *cbgt.NodeDef, key string) (string, error) {
	var bindPort string
	bindValue, err := nodeDef.GetFromParsedExtras(key)
	if err == nil && bindValue != nil {
		if bindStr, ok := bindValue.(string); ok {
			portPos := strings.LastIndex(bindStr, ":") + 1
			if portPos > 0 && portPos < len(bindStr) {
				bindPort = bindStr[portPos:]
			}
		}
	}
	return bindPort, err
}

// GroupGrpcClientsByHostPort groups the gRPC clients by their
// HostPort, merging the pindexNames.  This is an enabler to allow
// scatter/gather to use fewer gRPC calls.
func GroupGrpcClientsByHostPort(clients []*GrpcClient) (rv []*GrpcClient) {
	m := map[string]*GrpcClient{}

	for _, client := range clients {
		groupByKey := client.HostPort +
			"/" + client.IndexName + "/" + client.IndexUUID

		c, exists := m[groupByKey]
		if !exists {
			c = &GrpcClient{
				Mgr:         client.Mgr,
				name:        groupByKey,
				HostPort:    client.HostPort,
				IndexName:   client.IndexName,
				IndexUUID:   client.IndexUUID,
				Consistency: client.Consistency,
				GrpcCli:     client.GrpcCli,
			}

			m[groupByKey] = c

			rv = append(rv, c)
		}

		c.PIndexNames = append(c.PIndexNames, client.PIndexNames...)
	}

	return rv
}