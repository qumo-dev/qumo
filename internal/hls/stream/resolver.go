package stream

import (
	"context"

	"github.com/okdaichi/qumo-ledger/ledger"
)

// SegmentResolver decides whether a segment is served by redirect.
//
// HLS and DASH both address segments by their [ledger.GroupID], so a segment
// request looks the same no matter which format pointed at it: the server
// resolves the GroupID to a [ledger.GroupInfo] and asks the resolver what to do.
// ResolveSegment returns a URL to redirect to; an empty URL means the server
// proxies the segment bytes itself through the ledger.
//
// Keeping delivery at this layer — rather than on the store — leaves the core
// independent of the strategy. Proxying is the simple default for local
// development, where the object store is local and there is nothing to sign; a
// production deployment redirects to signed object URLs, and does so without the
// store gaining a presigning method it could not implement uniformly across
// backends.
type SegmentResolver interface {
	ResolveSegment(ctx context.Context, group ledger.GroupInfo) (string, error)
}

// ProxyResolver never redirects: the server proxies segment bytes through the
// ledger. It is the zero-value default for local development and simple
// deployments.
type ProxyResolver struct{}

// ResolveSegment implements [SegmentResolver] by never redirecting.
func (ProxyResolver) ResolveSegment(context.Context, ledger.GroupInfo) (string, error) {
	return "", nil
}

// RedirectResolver mints a URL for each segment. It is satisfied by a function,
// so a production deployment wires S3 or GCS presigning against
// [ledger.GroupInfo.ObjectKey] — the object key — with no cloud SDK imported
// here and no method added to the store:
//
//	h, _ := stream.NewHandler(track, stream.Options{
//	    Resolver: stream.RedirectResolver(func(g ledger.GroupInfo) string {
//	        return presigner.GetSignedURL(g.ObjectKey)
//	    }),
//	})
type RedirectResolver func(ledger.GroupInfo) string

// ResolveSegment implements [SegmentResolver] by returning the minted URL.
func (f RedirectResolver) ResolveSegment(_ context.Context, g ledger.GroupInfo) (string, error) {
	return f(g), nil
}

var _ SegmentResolver = ProxyResolver{}
var _ SegmentResolver = RedirectResolver(nil)
