package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/edynasty/LumenCortex/internal/verify"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestProviderReviewerKeepsUsageOnRejectedResponse(t *testing.T) {
	for _, failedCall := range []bool{false, true} {
		t.Run(map[bool]string{false: "unparsable", true: "provider error"}[failedCall], func(t *testing.T) {
			engine := openTestEngine(t)
			p := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				response := protocol.ProviderResponse{Message: protocol.Message{Content: "not JSON"}, Usage: protocol.Usage{PromptTokens: 23, CompletionTokens: 7, TotalTokens: 30}}
				if failedCall {
					return response, errors.New("broken response")
				}
				return response, nil
			}}
			id := newParentSession(t, engine, completingProvider("unused"))
			ref := bind(engine, p)
			reviewer := providerReviewer{engine: engine, sessionID: id, model: ref}
			result, err := reviewer.Review(context.Background(), verify.ReviewRequest{SessionID: id, Goal: "check file", Final: "done"})
			if err == nil {
				t.Fatal("invalid response accepted")
			}
			if result.Model != string(ref) || result.RequestID == "" || result.Usage.TotalTokens != 30 || result.Usage.PromptTokens != 23 {
				t.Fatalf("lost admitted request evidence: %+v", result)
			}
		})
	}
}
