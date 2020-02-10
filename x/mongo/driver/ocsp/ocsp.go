// Copyright (C) MongoDB, Inc. 2017-present.
//
// Licensed under the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License. You may obtain
// a copy of the License at http://www.apache.org/licenses/LICENSE-2.0

package ocsp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/asn1"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/crypto/ocsp"
	"golang.org/x/sync/errgroup"
)

var (
	mustStapleExtensionOID = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 24}
	ocspSigningExtensionID = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 9}

	defaultRequestTimeout = 5 * time.Second
	errGotOCSPResponse    = errors.New("done")
)

// Error represents an OCSP verification error
type Error struct {
	wrapped error
}

// Error implements the error interface
func (e *Error) Error() string {
	return fmt.Sprintf("OCSP verification failed: %v", e.wrapped)
}

func newOCSPError(wrapped error) error {
	return &Error{wrapped: wrapped}
}

// Verify performs OCSP verification for the provided ConnectionState instance.
func Verify(ctx context.Context, connState tls.ConnectionState) error {
	if len(connState.VerifiedChains) == 0 {
		return newOCSPError(errors.New("no verified certificate chains reported after TLS handshake"))
	}

	certChain := connState.VerifiedChains[0]
	if numCerts := len(certChain); numCerts == 0 {
		return newOCSPError(errors.New("verified chain contained no certificates"))
	}

	ocspCfg, err := newConfig(certChain)
	if err != nil {
		return newOCSPError(err)
	}

	res, err := parseStaple(ocspCfg, connState.OCSPResponse)
	if err != nil {
		return newOCSPError(err)
	}
	if res == nil {
		// If there was no staple, contact responders.
		res, err = contactResponders(ctx, ocspCfg)
		if err != nil {
			return newOCSPError(err)
		}
	}
	if res == nil {
		// If no response was parsed from the staple and responders, the status of the certificate is unknown, so don't
		// error.
		return nil
	}

	if err = verifyResponse(ocspCfg, res); err != nil {
		return newOCSPError(err)
	}
	return nil
}

// parseStaple returns the OCSP response from the provided staple. An error will be returned if any of the following are
// true:
//
// 1. cfg.serverCert has the Must-Staple extension but the staple is empty.
// 2. The staple is malformed.
// 3. The staple does not cover cfg.serverCert.
// 4. The OCSP response has an error status.
func parseStaple(cfg config, staple []byte) (*ocsp.Response, error) {
	var mustStaple bool
	for _, extension := range cfg.serverCert.Extensions {
		if extension.Id.Equal(mustStapleExtensionOID) {
			mustStaple = true
			break
		}
	}

	// If the server has a Must-Staple certificate and the server does not present a stapled OCSP response, error.
	if mustStaple && len(staple) == 0 {
		return nil, errors.New("server provided a certificate with the Must-Staple extension but did not " +
			"provde a stapled OCSP response")
	}

	if len(staple) == 0 {
		return nil, nil
	}

	parsedResponse, err := ocsp.ParseResponseForCert(staple, cfg.serverCert, cfg.issuer)
	if err != nil {
		// If the stapled response could not be parsed correctly, error. This can happen if the response is malformed,
		// the response does not cover the certificate presented by the server, or if the response contains an error
		// status.
		return nil, fmt.Errorf("error parsing stapled response: %v", err)
	}
	return parsedResponse, nil
}

// contactResponders will send a request to the OCSP responders reported by cfg.serverCert. The first response that
// conclusively identifies cfg.serverCert as good or revoked will be returned. If all responders are unavailable or no
// responder returns a conclusive status, (nil, nil) will be returned.
func contactResponders(ctx context.Context, cfg config) (*ocsp.Response, error) {
	if len(cfg.serverCert.OCSPServer) == 0 {
		return nil, nil
	}

	requestBytes, err := ocsp.CreateRequest(cfg.serverCert, cfg.issuer, nil)
	if err != nil {
		return nil, nil
	}

	requestCtx := ctx // Either ctx or a new context derived from ctx with a five second timeout.
	userContextUsed := true
	var cancelFn context.CancelFunc

	// Use a context with defaultRequestTimeout if ctx does not have a deadline set or the current deadline is further
	// out than defaultRequestTimeout. If the current deadline is less than less than defaultRequestTimeout out, respect
	// it. Calling context.WithTimeout would do this for us, but we need to know which context we're using.
	wantDeadline := time.Now().Add(defaultRequestTimeout)
	if deadline, ok := ctx.Deadline(); !ok || deadline.After(wantDeadline) {
		userContextUsed = false
		requestCtx, cancelFn = context.WithDeadline(ctx, wantDeadline)
	}
	defer func() {
		if cancelFn != nil {
			cancelFn()
		}
	}()

	group, groupCtx := errgroup.WithContext(requestCtx)
	ocspResponses := make(chan *ocsp.Response, len(cfg.serverCert.OCSPServer))
	defer close(ocspResponses)

	for _, endpoint := range cfg.serverCert.OCSPServer {
		// Re-assign endpoint so it gets re-scoped rather than using the iteration variable in the goroutine. See
		// https://golang.org/doc/faq#closures_and_goroutines.
		endpoint := endpoint
		group.Go(func() error {
			// Use bytes.NewReader instead of bytes.NewBuffer because a bytes.Buffer is an owning representation and the
			// docs recommend not using the underlying []byte after creating the buffer, so a new copy of requestBytes
			// would be needed for each request.
			request, err := http.NewRequest("POST", endpoint, bytes.NewReader(requestBytes))
			if err != nil {
				return nil
			}
			request = request.WithContext(groupCtx)

			// Execute the request and handle errors as follows:
			//
			// 1. If the original context expired or was cancelled, propagate the error up so the caller will abort the
			// verification and return control to the user.
			//
			// 2. If any other errors occurred, including the defaultRequestTimeout expiring, or the response has a
			// non-200 status code, suppress the error because we want to ignore this responder and wait for a different
			// one to responsd.
			httpResponse, err := http.DefaultClient.Do(request)
			if err != nil {
				urlErr, ok := err.(*url.Error)
				if !ok {
					return nil
				}

				timeout := urlErr.Timeout()
				cancelled := urlErr.Err == context.Canceled // Timeout() does not return true for context.Cancelled.
				if userContextUsed && (timeout || cancelled) {
					// Handle the original context expiring or being cancelled.
					return newOCSPError(err)
				}
				return nil // Ignore all other errors.
			}
			defer func() {
				_ = httpResponse.Body.Close()
			}()
			if httpResponse.StatusCode != 200 {
				return nil
			}

			httpBytes, err := ioutil.ReadAll(httpResponse.Body)
			if err != nil {
				return nil
			}

			ocspResponse, err := ocsp.ParseResponseForCert(httpBytes, cfg.serverCert, cfg.issuer)
			if err != nil || ocspResponse.Status == ocsp.Unknown {
				// If there was an error parsing the response or the response was inconclusive, suppress the error
				// because we want to ignore this responder.
				return nil
			}

			// Store the response and return a sentinel error so the error group will exit and any in-flight requests
			// will be cancelled.
			ocspResponses <- ocspResponse
			return errGotOCSPResponse
		})
	}

	if err := group.Wait(); err != nil && err != errGotOCSPResponse {
		return nil, err
	}
	if len(ocspResponses) == 0 {
		// None of the responders gave a conclusive response.
		return nil, nil
	}
	return <-ocspResponses, nil
}

// verifyResponse checks that the provided OCSP response is valid. An error is returned if the response is invalid or
// reports that the certificate being checked has been revoked.
func verifyResponse(cfg config, res *ocsp.Response) error {
	currTime := time.Now().UTC()
	if res.ThisUpdate.After(currTime) {
		return fmt.Errorf("reported thisUpdate time %s is after current time %s", res.ThisUpdate, currTime)
	}
	if !res.NextUpdate.IsZero() && res.NextUpdate.Before(currTime) {
		return fmt.Errorf("reported nextUpdate time %s is before current time %s", res.NextUpdate, currTime)
	}
	if res.Status == ocsp.Revoked {
		return errors.New("certificate is revoked")
	}
	return nil
}
