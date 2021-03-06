package exchange

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/mxmCherry/openrtb"
	nativeRequests "github.com/mxmCherry/openrtb/native/request"
	nativeResponse "github.com/mxmCherry/openrtb/native/response"
	"github.com/prebid/prebid-server/adapters"
	"github.com/prebid/prebid-server/currencies"
	"github.com/prebid/prebid-server/errortypes"
	"github.com/prebid/prebid-server/openrtb_ext"
	"golang.org/x/net/context/ctxhttp"
)

// adaptedBidder defines the contract needed to participate in an Auction within an Exchange.
//
// This interface exists to help segregate core auction logic.
//
// Any logic which can be done _within a single Seat_ goes inside one of these.
// Any logic which _requires responses from all Seats_ goes inside the Exchange.
//
// This interface differs from adapters.Bidder to help minimize code duplication across the
// adapters.Bidder implementations.
type adaptedBidder interface {
	// requestBid fetches bids for the given request.
	//
	// An adaptedBidder *may* return two non-nil values here. Errors should describe situations which
	// make the bid (or no-bid) "less than ideal." Common examples include:
	//
	// 1. Connection issues.
	// 2. Imps with Media Types which this Bidder doesn't support.
	// 3. The Context timeout expired before all expected bids were returned.
	// 4. The Server sent back an unexpected Response, so some bids were ignored.
	//
	// Any errors will be user-facing in the API.
	// Error messages should help publishers understand what might account for "bad" bids.
	requestBid(ctx context.Context, request *openrtb.BidRequest, name openrtb_ext.BidderName, bidAdjustment float64, conversions currencies.Conversions, reqInfo *adapters.ExtraRequestInfo) (*pbsOrtbSeatBid, []error)
}

// pbsOrtbBid is a Bid returned by an adaptedBidder.
//
// pbsOrtbBid.bid.Ext will become "response.seatbid[i].bid.ext.bidder" in the final OpenRTB response.
// pbsOrtbBid.bidType will become "response.seatbid[i].bid.ext.prebid.type" in the final OpenRTB response.
// pbsOrtbBid.bidTargets does not need to be filled out by the Bidder. It will be set later by the exchange.
// pbsOrtbBid.bidVideo is optional but should be filled out by the Bidder if bidType is video.
// pbsOrtbBid.dealPriority will become "response.seatbid[i].bid.dealPriority" in the final OpenRTB response.
type pbsOrtbBid struct {
	bid          *openrtb.Bid
	bidType      openrtb_ext.BidType
	bidTargets   map[string]string
	bidVideo     *openrtb_ext.ExtBidPrebidVideo
	dealPriority int
}

// pbsOrtbSeatBid is a SeatBid returned by an adaptedBidder.
//
// This is distinct from the openrtb.SeatBid so that the prebid-server ext can be passed back with typesafety.
type pbsOrtbSeatBid struct {
	// bids is the list of bids which this adaptedBidder wishes to make.
	bids []*pbsOrtbBid
	// currency is the currency in which the bids are made.
	// Should be a valid currency ISO code.
	currency string
	// httpCalls is the list of debugging info. It should only be populated if the request.test == 1.
	// This will become response.ext.debug.httpcalls.{bidder} on the final Response.
	httpCalls []*openrtb_ext.ExtHttpCall
	// ext contains the extension for this seatbid.
	// if len(bids) > 0, this will become response.seatbid[i].ext.{bidder} on the final OpenRTB response.
	// if len(bids) == 0, this will be ignored because the OpenRTB spec doesn't allow a SeatBid with 0 Bids.
	ext json.RawMessage
}

// adaptBidder converts an adapters.Bidder into an exchange.adaptedBidder.
//
// The name refers to the "Adapter" architecture pattern, and should not be confused with a Prebid "Adapter"
// (which is being phased out and replaced by Bidder for OpenRTB auctions)
func adaptBidder(bidder adapters.Bidder, client *http.Client) adaptedBidder {
	return &bidderAdapter{
		Bidder: bidder,
		Client: client,
	}
}

type bidderAdapter struct {
	Bidder adapters.Bidder
	Client *http.Client
}

func (bidder *bidderAdapter) requestBid(ctx context.Context, request *openrtb.BidRequest, name openrtb_ext.BidderName, bidAdjustment float64, conversions currencies.Conversions, reqInfo *adapters.ExtraRequestInfo) (*pbsOrtbSeatBid, []error) {
	reqData, errs := bidder.Bidder.MakeRequests(request, reqInfo)

	if len(reqData) == 0 {
		// If the adapter failed to generate both requests and errors, this is an error.
		if len(errs) == 0 {
			errs = append(errs, &errortypes.FailedToRequestBids{Message: "The adapter failed to generate any bid requests, but also failed to generate an error explaining why"})
		}
		return nil, errs
	}

	// Make any HTTP requests in parallel.
	// If the bidder only needs to make one, save some cycles by just using the current one.
	responseChannel := make(chan *httpCallInfo, len(reqData))
	if len(reqData) == 1 {
		responseChannel <- bidder.doRequest(ctx, reqData[0])
	} else {
		for _, oneReqData := range reqData {
			go func(data *adapters.RequestData) {
				responseChannel <- bidder.doRequest(ctx, data)
			}(oneReqData) // Method arg avoids a race condition on oneReqData
		}
	}

	defaultCurrency := "USD"
	seatBid := &pbsOrtbSeatBid{
		bids:      make([]*pbsOrtbBid, 0, len(reqData)),
		currency:  defaultCurrency,
		httpCalls: make([]*openrtb_ext.ExtHttpCall, 0, len(reqData)),
	}

	// If the bidder made multiple requests, we still want them to enter as many bids as possible...
	// even if the timeout occurs sometime halfway through.
	for i := 0; i < len(reqData); i++ {
		httpInfo := <-responseChannel
		// If this is a test bid, capture debugging info from the requests.
		if request.Test == 1 {
			seatBid.httpCalls = append(seatBid.httpCalls, makeExt(httpInfo))
		}

		if httpInfo.err == nil {
			bidResponse, moreErrs := bidder.Bidder.MakeBids(request, httpInfo.request, httpInfo.response)
			errs = append(errs, moreErrs...)

			if bidResponse != nil {
				// Setup default currency as `USD` is not set in bid request nor bid response
				if bidResponse.Currency == "" {
					bidResponse.Currency = defaultCurrency
				}
				if len(request.Cur) == 0 {
					request.Cur = []string{defaultCurrency}
				}

				// Try to get a conversion rate
				// Try to get the first currency from request.cur having a match in the rate converter,
				// and use it as currency
				var conversionRate float64
				var err error
				for _, bidReqCur := range request.Cur {
					if conversionRate, err = conversions.GetRate(bidResponse.Currency, bidReqCur); err == nil {
						seatBid.currency = bidReqCur
						break
					}
				}

				// Only do this for request from mobile app
				if request.App != nil {
					for i := 0; i < len(bidResponse.Bids); i++ {
						if bidResponse.Bids[i].BidType == openrtb_ext.BidTypeNative {
							nativeMarkup, moreErrs := addNativeTypes(bidResponse.Bids[i].Bid, request)
							errs = append(errs, moreErrs...)

							if nativeMarkup != nil {
								markup, err := json.Marshal(*nativeMarkup)
								if err != nil {
									errs = append(errs, err)
								} else {
									bidResponse.Bids[i].Bid.AdM = string(markup)
								}
							}
						}
					}
				}

				if err == nil {
					// Conversion rate found, using it for conversion
					for i := 0; i < len(bidResponse.Bids); i++ {
						if bidResponse.Bids[i].Bid != nil {
							bidResponse.Bids[i].Bid.Price = bidResponse.Bids[i].Bid.Price * bidAdjustment * conversionRate
						}
						seatBid.bids = append(seatBid.bids, &pbsOrtbBid{
							bid:          bidResponse.Bids[i].Bid,
							bidType:      bidResponse.Bids[i].BidType,
							bidVideo:     bidResponse.Bids[i].BidVideo,
							dealPriority: bidResponse.Bids[i].DealPriority,
						})
					}
				} else {
					// If no conversions found, do not handle the bid
					errs = append(errs, err)
				}
			}
		} else {
			errs = append(errs, httpInfo.err)
		}
	}

	return seatBid, errs
}

func addNativeTypes(bid *openrtb.Bid, request *openrtb.BidRequest) (*nativeResponse.Response, []error) {
	var errs []error
	var nativeMarkup *nativeResponse.Response
	if err := json.Unmarshal(json.RawMessage(bid.AdM), &nativeMarkup); err != nil || len(nativeMarkup.Assets) == 0 {
		// Some bidders are returning non-IAB compliant native markup. In this case Prebid server will not be able to add types. E.g Facebook
		return nil, errs
	}

	nativeImp, err := getNativeImpByImpID(bid.ImpID, request)
	if err != nil {
		errs = append(errs, err)
		return nil, errs
	}

	var nativePayload nativeRequests.Request
	if err := json.Unmarshal(json.RawMessage((*nativeImp).Request), &nativePayload); err != nil {
		errs = append(errs, err)
	}

	for _, asset := range nativeMarkup.Assets {
		if err := setAssetTypes(asset, nativePayload); err != nil {
			errs = append(errs, err)
		}
	}

	return nativeMarkup, errs
}

func setAssetTypes(asset nativeResponse.Asset, nativePayload nativeRequests.Request) error {
	if asset.Img != nil {
		if tempAsset, err := getAssetByID(asset.ID, nativePayload.Assets); err == nil {
			if tempAsset.Img != nil {
				if tempAsset.Img.Type != 0 {
					asset.Img.Type = tempAsset.Img.Type
				}
			} else {
				return fmt.Errorf("Response has an Image asset with ID:%d present that doesn't exist in the request", asset.ID)
			}
		} else {
			return err
		}
	}

	if asset.Data != nil {
		if tempAsset, err := getAssetByID(asset.ID, nativePayload.Assets); err == nil {
			if tempAsset.Data != nil {
				if tempAsset.Data.Type != 0 {
					asset.Data.Type = tempAsset.Data.Type
				}
			} else {
				return fmt.Errorf("Response has a Data asset with ID:%d present that doesn't exist in the request", asset.ID)
			}
		} else {
			return err
		}
	}
	return nil
}

func getNativeImpByImpID(impID string, request *openrtb.BidRequest) (*openrtb.Native, error) {
	for _, impInRequest := range request.Imp {
		if impInRequest.ID == impID && impInRequest.Native != nil {
			return impInRequest.Native, nil
		}
	}
	return nil, errors.New("Could not find native imp")
}

func getAssetByID(id int64, assets []nativeRequests.Asset) (nativeRequests.Asset, error) {
	for _, asset := range assets {
		if id == asset.ID {
			return asset, nil
		}
	}
	return nativeRequests.Asset{}, fmt.Errorf("Unable to find asset with ID:%d in the request", id)
}

// makeExt transforms information about the HTTP call into the contract class for the PBS response.
func makeExt(httpInfo *httpCallInfo) *openrtb_ext.ExtHttpCall {
	if httpInfo.err == nil {
		return &openrtb_ext.ExtHttpCall{
			Uri:          httpInfo.request.Uri,
			RequestBody:  string(httpInfo.request.Body),
			ResponseBody: string(httpInfo.response.Body),
			Status:       httpInfo.response.StatusCode,
		}
	} else if httpInfo.request == nil {
		return &openrtb_ext.ExtHttpCall{}
	} else {
		return &openrtb_ext.ExtHttpCall{
			Uri:         httpInfo.request.Uri,
			RequestBody: string(httpInfo.request.Body),
		}
	}
}

// doRequest makes a request, handles the response, and returns the data needed by the
// Bidder interface.
func (bidder *bidderAdapter) doRequest(ctx context.Context, req *adapters.RequestData) *httpCallInfo {
	httpReq, err := http.NewRequest(req.Method, req.Uri, bytes.NewBuffer(req.Body))
	if err != nil {
		return &httpCallInfo{
			request: req,
			err:     err,
		}
	}
	httpReq.Header = req.Headers

	httpResp, err := ctxhttp.Do(ctx, bidder.Client, httpReq)
	if err != nil {
		if err == context.DeadlineExceeded {
			err = &errortypes.Timeout{Message: err.Error()}
			if tb, ok := bidder.Bidder.(adapters.TimeoutBidder); ok {
				// Toss the timeout notification call into a go routine, as we are out of time'
				// and cannot delay processing. We don't do anything result, as there is not much
				// we can do about a timeout notification failure. We do not want to get stuck in
				// a loop of trying to report timeouts to the timeout notifications.
				go bidder.doTimeoutNotification(tb, req)
			}

		}
		return &httpCallInfo{
			request: req,
			err:     err,
		}
	}

	respBody, err := ioutil.ReadAll(httpResp.Body)
	if err != nil {
		return &httpCallInfo{
			request: req,
			err:     err,
		}
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 400 {
		err = &errortypes.BadServerResponse{
			Message: fmt.Sprintf("Server responded with failure status: %d. Set request.test = 1 for debugging info.", httpResp.StatusCode),
		}
	}

	return &httpCallInfo{
		request: req,
		response: &adapters.ResponseData{
			StatusCode: httpResp.StatusCode,
			Body:       respBody,
			Headers:    httpResp.Header,
		},
		err: err,
	}
}

func (bidder *bidderAdapter) doTimeoutNotification(timeoutBidder adapters.TimeoutBidder, req *adapters.RequestData) {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	toReq, errL := timeoutBidder.MakeTimeoutNotification(req)
	if toReq != nil && len(errL) == 0 {
		httpReq, err := http.NewRequest(toReq.Method, toReq.Uri, bytes.NewBuffer(toReq.Body))
		if err == nil {
			httpReq.Header = req.Headers
			ctxhttp.Do(ctx, bidder.Client, httpReq)
			// No validation yet on sending notifications
		}
	}

}

type httpCallInfo struct {
	request  *adapters.RequestData
	response *adapters.ResponseData
	err      error
}
