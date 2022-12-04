// Copyright 2022 Northern.tech AS
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

package reporting

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/mendersoftware/go-lib-micro/log"

	"github.com/mendersoftware/reporting/client/inventory"
	"github.com/mendersoftware/reporting/mapping"
	"github.com/mendersoftware/reporting/model"
	"github.com/mendersoftware/reporting/store"
)

//go:generate ../../x/mockgen.sh
type App interface {
	HealthCheck(ctx context.Context) error
	GetSearchableInvAttrs(ctx context.Context, tid string) ([]model.FilterAttribute, error)
	InventorySearchDevices(ctx context.Context, searchParams *model.SearchParams) (
		[]inventory.Device, int, error)
}

type app struct {
	store  store.Store
	mapper mapping.Mapper
	ds     store.DataStore
}

func NewApp(store store.Store, ds store.DataStore) App {
	mapper := mapping.NewMapper(ds)
	return &app{
		store:  store,
		mapper: mapper,
		ds:     ds,
	}
}

// HealthCheck performs a health check and returns an error if it fails
func (a *app) HealthCheck(ctx context.Context) error {
	err := a.ds.Ping(ctx)
	if err == nil {
		err = a.store.Ping(ctx)
	}
	return err
}

func (app *app) InventorySearchDevices(
	ctx context.Context,
	searchParams *model.SearchParams,
) ([]inventory.Device, int, error) {
	if err := app.mapSearchParamsAttributes(ctx, searchParams); err != nil {
		return nil, 0, err
	}
	query, err := model.BuildQuery(*searchParams)
	if err != nil {
		return nil, 0, err
	}

	if searchParams.TenantID != "" {
		query = query.Must(model.M{
			"term": model.M{
				model.FieldNameTenantID: searchParams.TenantID,
			},
		})
	}

	if len(searchParams.DeviceIDs) > 0 {
		query = query.Must(model.M{
			"terms": model.M{
				model.FieldNameID: searchParams.DeviceIDs,
			},
		})
	}

	esRes, err := app.store.Search(ctx, query)
	if err != nil {
		return nil, 0, err
	}

	res, total, err := app.storeToInventoryDevs(ctx, searchParams.TenantID, esRes)
	if err != nil {
		return nil, 0, err
	}

	return res, total, err
}

func (app *app) mapSearchParamsAttributes(ctx context.Context,
	searchParams *model.SearchParams) error {
	if len(searchParams.Attributes) > 0 {
		attributes := make(inventory.DeviceAttributes, 0, len(searchParams.Attributes))
		for i := 0; i < len(searchParams.Attributes); i++ {
			attributes = append(attributes, inventory.DeviceAttribute{
				Name:  searchParams.Attributes[i].Attribute,
				Scope: searchParams.Attributes[i].Scope,
			})
		}
		attributes, err := app.mapper.MapInventoryAttributes(ctx, searchParams.TenantID,
			attributes, false)
		if err != nil {
			return err
		}
		searchParams.Attributes = make([]model.SelectAttribute, 0, len(searchParams.Attributes))
		for _, attribute := range attributes {
			searchParams.Attributes = append(searchParams.Attributes, model.SelectAttribute{
				Attribute: attribute.Name,
				Scope:     attribute.Scope,
			})
		}
	}
	return nil
}

// storeToInventoryDevs translates ES results directly to iventory devices
func (a *app) storeToInventoryDevs(
	ctx context.Context, tenantID string, storeRes map[string]interface{},
) ([]inventory.Device, int, error) {
	devs := []inventory.Device{}

	hitsM, ok := storeRes["hits"].(map[string]interface{})
	if !ok {
		return nil, 0, errors.New("can't process store hits map")
	}

	hitsTotalM, ok := hitsM["total"].(map[string]interface{})
	if !ok {
		return nil, 0, errors.New("can't process total hits struct")
	}

	total, ok := hitsTotalM["value"].(float64)
	if !ok {
		return nil, 0, errors.New("can't process total hits value")
	}

	hitsS, ok := hitsM["hits"].([]interface{})
	if !ok {
		return nil, 0, errors.New("can't process store hits slice")
	}

	for _, v := range hitsS {
		res, err := a.storeToInventoryDev(ctx, tenantID, v)
		if err != nil {
			return nil, 0, err
		}

		devs = append(devs, *res)
	}

	return devs, int(total), nil
}

func (a *app) storeToInventoryDev(ctx context.Context, tenantID string,
	storeRes interface{}) (*inventory.Device, error) {
	resM, ok := storeRes.(map[string]interface{})
	if !ok {
		return nil, errors.New("can't process individual hit")
	}

	// if query has a 'fields' clause, use 'fields' instead of '_source'
	sourceM, ok := resM["_source"].(map[string]interface{})
	if !ok {
		sourceM, ok = resM["fields"].(map[string]interface{})
		if !ok {
			return nil, errors.New("can't process hit's '_source' nor 'fields'")
		}
	}

	// if query has a 'fields' clause, all results will be arrays incl. device id, so extract it
	id, ok := sourceM["id"].(string)
	if !ok {
		idarr, ok := sourceM["id"].([]interface{})
		if !ok {
			return nil, errors.New(
				"can't parse device id as neither single value nor array",
			)
		}

		id, ok = idarr[0].(string)
		if !ok {
			return nil, errors.New(
				"can't parse device id as neither single value nor array",
			)
		}
	}

	ret := &inventory.Device{
		ID: inventory.DeviceID(id),
	}

	attrs := []inventory.DeviceAttribute{}

	for k, v := range sourceM {
		s, n, err := model.MaybeParseAttr(k)
		if err != nil {
			return nil, err
		}

		if vArray, ok := v.([]interface{}); ok && len(vArray) == 1 {
			v = vArray[0]
		}

		if n != "" {
			a := inventory.DeviceAttribute{
				Name:  model.Redot(n),
				Scope: s,
				Value: v,
			}

			if a.Scope == model.ScopeSystem &&
				a.Name == model.AttrNameUpdatedAt {
				ret.UpdatedTs = parseTime(v)
			} else if a.Scope == model.ScopeSystem &&
				a.Name == model.AttrNameCreatedAt {
				ret.CreatedTs = parseTime(v)
			}

			attrs = append(attrs, a)
		}
	}

	attributes, err := a.mapper.ReverseInventoryAttributes(ctx, tenantID, attrs)
	if err != nil {
		return nil, err
	}
	ret.Attributes = attributes

	return ret, nil
}

func parseTime(v interface{}) time.Time {
	val, _ := v.(string)
	if t, err := time.Parse(time.RFC3339, val); err == nil {
		return t
	}
	return time.Time{}
}

func (app *app) GetSearchableInvAttrs(
	ctx context.Context,
	tid string,
) ([]model.FilterAttribute, error) {
	l := log.FromContext(ctx)

	index, err := app.store.GetDevicesIndexMapping(ctx, tid)
	if err != nil {
		return nil, err
	}

	// inventory attributes are under 'mappings.properties'
	mappings, ok := index["mappings"]
	if !ok {
		return nil, errors.New("can't parse index mappings")
	}

	mappingsM, ok := mappings.(map[string]interface{})
	if !ok {
		return nil, errors.New("can't parse index mappings")
	}

	props, ok := mappingsM["properties"]
	if !ok {
		return nil, errors.New("can't parse index properties")
	}

	propsM, ok := props.(map[string]interface{})
	if !ok {
		return nil, errors.New("can't parse index properties")
	}

	ret := []model.FilterAttribute{}

	for k := range propsM {
		s, n, err := model.MaybeParseAttr(k)

		if err != nil {
			return nil, err
		}

		if n != "" {
			ret = append(ret, model.FilterAttribute{Name: n, Scope: s, Count: 1})
		}
	}

	sort.Slice(ret, func(i, j int) bool {
		if ret[j].Scope > ret[i].Scope {
			return true
		}

		if ret[j].Scope < ret[i].Scope {
			return false
		}

		return ret[j].Name > ret[i].Name
	})

	l.Debugf("parsed searchable attributes %v\n", ret)

	return ret, nil
}
