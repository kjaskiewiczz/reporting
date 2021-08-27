// Copyright 2021 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
package inventory

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/mendersoftware/go-lib-micro/log"
	"github.com/pkg/errors"

	"github.com/mendersoftware/reporting/model"
)

const (
	urlSearch      = "/api/internal/v2/inventory/tenants/:tid/filters/search"
	defaultTimeout = 10 * time.Second
)

//go:generate ../../x/mockgen.sh
type Client interface {
	//GetDevices uses the search endpoint to get devices just by ids (not filters)
	GetDevices(ctx context.Context, tid string, deviceIDs []string) ([]model.InvDevice, error)
}

type client struct {
	client  *http.Client
	urlBase string
}

func NewClient(urlBase string, skipVerify bool) Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: skipVerify},
	}

	return &client{
		client: &http.Client{
			Transport: tr,
		},
		urlBase: urlBase,
	}
}

func (c *client) GetDevices(
	ctx context.Context,
	tid string,
	deviceIDs []string,
) ([]model.InvDevice, error) {
	l := log.FromContext(ctx)

	getReq := &GetDevsReq{
		DeviceIDs: deviceIDs,
	}

	body, err := json.Marshal(getReq)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to serialize get devices request")
	}

	rd := bytes.NewReader(body)

	url := joinURL(c.urlBase, urlSearch)
	url = strings.Replace(url, ":tid", tid, 1)

	ctx, cancel := context.WithTimeout(ctx, defaultTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, rd)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create request")
	}

	req.Header.Set("Content-Type", "application/json")

	rsp, err := c.client.Do(req)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to submit %s %s", req.Method, req.URL)
	}
	defer rsp.Body.Close()

	if rsp.StatusCode != http.StatusOK {
		l.Errorf("request %s %s failed with status %v, response: %s",
			req.Method, req.URL, rsp.Status, body)

		return nil, errors.Errorf(
			"%s %s request failed with status %v", req.Method, req.URL, rsp.Status)
	}

	dec := json.NewDecoder(rsp.Body)
	var invDevs []model.InvDevice
	if err = dec.Decode(&invDevs); err != nil {
		return nil, errors.Wrap(err, "failed to parse request body")
	}

	return invDevs, nil
}

func joinURL(base, url string) string {
	url = strings.TrimPrefix(url, "/")
	if !strings.HasSuffix(base, "/") {
		base = base + "/"
	}
	return base + url

}
