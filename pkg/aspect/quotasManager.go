// Copyright 2017 Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package aspect

import (
	"fmt"
	"strconv"
	"sync/atomic"

	ptypes "github.com/gogo/protobuf/types"

	dpb "istio.io/api/mixer/v1/config/descriptor"
	"istio.io/mixer/pkg/adapter"
	aconfig "istio.io/mixer/pkg/aspect/config"
	"istio.io/mixer/pkg/attribute"
	"istio.io/mixer/pkg/config"
	"istio.io/mixer/pkg/expr"
	"istio.io/mixer/pkg/status"
)

type (
	quotasManager struct {
		dedupCounter int64
	}

	quotaInfo struct {
		definition *adapter.QuotaDefinition
		labels     map[string]string
	}

	quotasWrapper struct {
		manager  *quotasManager
		aspect   adapter.QuotasAspect
		metadata map[string]*quotaInfo
	}
)

// newQuotasManager returns a manager for the quotas aspect.
func newQuotasManager() Manager {
	return &quotasManager{}
}

// NewAspect creates a quota aspect.
func (m *quotasManager) NewAspect(c *config.Combined, a adapter.Builder, env adapter.Env) (Wrapper, error) {
	params := c.Aspect.Params.(*aconfig.QuotasParams)

	// TODO: get this from config
	if len(params.Quotas) == 0 {
		params = &aconfig.QuotasParams{
			Quotas: []*aconfig.QuotasParams_Quota{
				{DescriptorName: "RequestCount"},
			},
		}
	}

	// TODO: get this from config
	desc := []dpb.QuotaDescriptor{
		{
			Name:       "RequestCount",
			MaxAmount:  5,
			Expiration: &ptypes.Duration{Seconds: 1},
		},
	}

	metadata := make(map[string]*quotaInfo, len(desc))
	defs := make(map[string]*adapter.QuotaDefinition, len(desc))
	for _, d := range desc {
		quota := findQuota(params.Quotas, d.Name)
		if quota == nil {
			env.Logger().Warningf("No quota found for descriptor %s, skipping it", d.Name)
			continue
		}

		// TODO: once we plumb descriptors into the validation, remove this err: no descriptor should make it through validation
		// if it cannot be converted into a QuotaDefinition, so we should never have to handle the error case.
		def, err := quotaDefinitionFromProto(&d)
		if err != nil {
			_ = env.Logger().Errorf("Failed to convert quota descriptor '%s' to definition with err: %s; skipping it.", d.Name, err)
			continue
		}

		defs[d.Name] = def
		metadata[d.Name] = &quotaInfo{
			labels:     quota.Labels,
			definition: def,
		}
	}

	asp, err := a.(adapter.QuotasBuilder).NewQuotasAspect(env, c.Builder.Params.(adapter.AspectConfig), defs)
	if err != nil {
		return nil, err
	}

	return &quotasWrapper{
		manager:  m,
		metadata: metadata,
		aspect:   asp,
	}, nil
}

func (*quotasManager) Kind() Kind                                                     { return QuotasKind }
func (*quotasManager) DefaultConfig() adapter.AspectConfig                            { return &aconfig.QuotasParams{} }
func (*quotasManager) ValidateConfig(adapter.AspectConfig) (ce *adapter.ConfigErrors) { return }

func (w *quotasWrapper) Execute(attrs attribute.Bag, mapper expr.Evaluator, ma APIMethodArgs) Output {
	qma, ok := ma.(*QuotaMethodArgs)

	// TODO: this conditional is only necessary because we currently perform quota
	// checking via the Check API, which doesn't generate a QuotaMethodArgs
	if !ok {
		qma = &QuotaMethodArgs{
			Quota:           "RequestCount",
			Amount:          1,
			DeduplicationID: strconv.FormatInt(atomic.AddInt64(&w.manager.dedupCounter, 1), 16),
			BestEffort:      false,
		}
	}

	info, ok := w.metadata[qma.Quota]
	if !ok {
		return Output{Status: status.WithInvalidArgument(fmt.Sprintf("unknown quota '%s'", qma.Quota))}
	}

	labels, err := evalAll(info.labels, attrs, mapper)
	if err != nil {
		return Output{Status: status.WithInvalidArgument(fmt.Sprintf("failed to evaluate labels for quota '%s' with err: %s", qma.Quota, err))}
	}

	qa := adapter.QuotaArgs{
		Definition:      info.definition,
		Labels:          labels,
		QuotaAmount:     qma.Amount,
		DeduplicationID: qma.DeduplicationID,
	}

	var amount int64

	if qma.BestEffort {
		amount, err = w.aspect.AllocBestEffort(qa)
	} else {
		amount, err = w.aspect.Alloc(qa)
	}

	if err != nil {
		return Output{Status: status.WithError(err)}
	}

	if amount == 0 {
		return Output{Status: status.WithResourceExhausted(fmt.Sprintf("Unable to allocate %v units from quota %s", amount, info.definition.Name))}
	}

	// TODO: need to return the allocated amount somehow in the Quota API's QuotaResponse message

	return Output{Status: status.OK}
}

func (w *quotasWrapper) Close() error {
	return w.aspect.Close()
}

func findQuota(quotas []*aconfig.QuotasParams_Quota, name string) *aconfig.QuotasParams_Quota {
	for _, q := range quotas {
		if q.DescriptorName == name {
			return q
		}
	}
	return nil
}

func quotaDefinitionFromProto(desc *dpb.QuotaDescriptor) (*adapter.QuotaDefinition, error) {
	labels := make(map[string]adapter.LabelType, len(desc.Labels))
	for _, label := range desc.Labels {
		l, err := valueTypeToLabelType(label.ValueType)
		if err != nil {
			return nil, fmt.Errorf("descriptor '%s' label '%s' failed to convert label type value '%v' from proto with err: %s",
				desc.Name, label.Name, label.ValueType, err)
		}
		labels[label.Name] = l
	}

	dur, _ := ptypes.DurationFromProto(desc.Expiration)
	return &adapter.QuotaDefinition{
		MaxAmount:   desc.MaxAmount,
		Expiration:  dur,
		Description: desc.Description,
		DisplayName: desc.DisplayName,
		Name:        desc.Name,
		Labels:      labels,
	}, nil
}
