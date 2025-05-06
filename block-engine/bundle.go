package block_engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	bundle_pb "github.com/Prophet-Solutions/jito-sdk/pb/bundle"
	searcher_pb "github.com/Prophet-Solutions/jito-sdk/pb/searcher"
	"github.com/Prophet-Solutions/jito-sdk/pkg"
	"github.com/gagliardetto/solana-go"
	"google.golang.org/grpc"
)

// Constants for retry and timeout configurations
const (
	CheckBundleRetries               = 10               // Number of times to retry checking bundle status
	CheckBundleRetryDelay            = 3 * time.Second  // Delay between retries for checking bundle status
	SignaturesConfirmationTimeout    = 15 * time.Second // Timeout for confirming signatures
	SignaturesConfirmationRetryDelay = 1 * time.Second  // Delay between retries for confirming signatures
)

// Helper method to check if a signature exists on the blockchain
func (c SearcherClient) checkSignatureExistence(ctx context.Context, signature solana.Signature) (bool, error) {
	// Make the RPC call to check the signature status
	time.Sleep(200 * time.Millisecond)
	out, err := c.RPCConn.GetSignatureStatuses(ctx, false, signature)
	if err != nil {
		return false, fmt.Errorf("error checking signature status: %v", err)
	}

	// Check if we have a valid result for the signature
	if out.Value == nil || len(out.Value) == 0 || out.Value[0] == nil {
		return false, fmt.Errorf("signature not found")
	}

	status := out.Value[0]
	if status == nil {
		return false, fmt.Errorf("signature status is nil")
	}

	// Check if the signature is confirmed
	if status.ConfirmationStatus == "confirmed" {
		return true, nil
	}

	// Handle failure cases
	if status.Err != nil {
		return false, fmt.Errorf("transaction failed: %v", status.Err)
	}

	// The signature isn't confirmed yet
	return false, nil
}

func (c SearcherClient) SendBundleWithConfirmation(
	ctx context.Context,
	transactions []*solana.Transaction,
	signature solana.Signature,
	opts ...grpc.CallOption,
) (BundleResponse, error) {
	resp, err := c.SendBundle(transactions, opts...)
	if err != nil {
		return BundleResponse{}, err
	}
	fmt.Printf("tx (%s) sent...\n", signature)

	// Check if the signature exists and is confirmed
	sigExists, err := c.checkSignatureExistence(ctx, signature)
	if err != nil {
		return BundleResponse{}, fmt.Errorf("error checking signature existence: %v", err)
	}

	if !sigExists {
		return BundleResponse{}, fmt.Errorf("signature %s has not been confirmed or failed", signature)
	}

	// Proceed with the normal confirmation process
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	type result struct {
		response *BundleResponse
		err      error
	}

	resultChan := make(chan result, 2)

	// Normal check goroutine
	go func() {
		defer close(resultChan)
		for i := 0; i < CheckBundleRetries; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}

			time.Sleep(CheckBundleRetryDelay)

			bundleResult, err := c.receiveBundleResult()
			if err != nil {
				log.Println("[normalCheck] receiveBundleResult error:", err)
				continue
			}

			if err := c.handleBundleResult(bundleResult); err != nil {
				if strings.Contains(err.Error(), "has already been processed") {
					resultChan <- result{&BundleResponse{
						BundleResponse: resp,
						Signatures:     pkg.BatchExtractSigFromTx(transactions),
					}, nil}
					return
				}
				log.Println("[normalCheck] handleBundleResult error:", err)
				continue
			}

			statuses, err := c.waitForSignatureStatuses(ctx, transactions)
			if err != nil {
				log.Println("[normalCheck] waitForSignatureStatuses error:", err)
				continue
			}

			if err := pkg.ValidateSignatureStatuses(statuses); err != nil {
				log.Println("[normalCheck] ValidateSignatureStatuses error:", err)
				continue
			}

			resultChan <- result{&BundleResponse{
				BundleResponse: resp,
				Signatures:     pkg.BatchExtractSigFromTx(transactions),
			}, nil}
			return
		}
	}()

	// Signature polling goroutine
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			out, err := c.RPCConn.GetSignatureStatuses(ctx, false, signature)
			if err != nil {
				log.Println("[additionalCheck] GetSignatureStatuses error:", err)
				continue
			}

			if out.Value == nil || len(out.Value) == 0 || out.Value[0] == nil {
				continue
			}

			for _, status := range out.Value {
				if status == nil {
					continue
				}
				if status.ConfirmationStatus == "confirmed" {
					resultChan <- result{&BundleResponse{
						BundleResponse: resp,
						Signatures:     pkg.BatchExtractSigFromTx(transactions),
					}, nil}
					return
				}
				if status.Err != nil {
					resultChan <- result{nil, fmt.Errorf("transaction failed: %v", status.Err)}
					return
				}
			}

			time.Sleep(CheckBundleRetryDelay)
		}
	}()

	// Wait for result or timeout
	select {
	case res := <-resultChan:
		return *res.response, res.err
	case <-ctx.Done():
		return BundleResponse{}, fmt.Errorf("SendBundleWithConfirmation: timeout exceeded")
	}
}

// SendBundle creates and sends a bundle of transactions to the Searcher service.
// It converts transactions to a protobuf packet and sends it using the SearcherService.
func (c *SearcherClient) SendBundle(
	transactions []*solana.Transaction,
	opts ...grpc.CallOption,
) (*searcher_pb.SendBundleResponse, error) {
	// Create a new bundle from the transactions
	bundle, err := c.NewBundle(transactions)
	if err != nil {
		return nil, err
	}

	// Send the bundle request to the Searcher service
	return c.SearcherService.SendBundle(
		c.AuthenticationService.GRPCCtx,
		&searcher_pb.SendBundleRequest{
			Bundle: bundle,
		},
		opts...,
	)
}

// NewBundle creates a new bundle protobuf object from a slice of transactions.
// It converts the transactions into protobuf packets and includes them in the bundle.
func (c *SearcherClient) NewBundle(transactions []*solana.Transaction) (*bundle_pb.Bundle, error) {
	// Convert the transactions to protobuf packets
	packets, err := pkg.ConvertBatchTransactionToProtobufPacket(transactions)
	if err != nil {
		return nil, err
	}

	// Create and return the bundle with the converted packets
	return &bundle_pb.Bundle{
		Packets: packets,
		Header:  nil,
	}, nil
}

// NewBundleSubscriptionResults subscribes to bundle result updates from the Searcher service.
// It uses the provided gRPC call options to set up the subscription.
func (c *SearcherClient) NewBundleSubscriptionResults(opts ...grpc.CallOption) (searcher_pb.SearcherService_SubscribeBundleResultsClient, error) {
	// Subscribe to bundle results from the Searcher service
	return c.SearcherService.SubscribeBundleResults(
		c.AuthenticationService.GRPCCtx,
		&searcher_pb.SubscribeBundleResultsRequest{},
		opts...,
	)
}
