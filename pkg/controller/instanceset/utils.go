/*
Copyright (C) 2022-2024 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package instanceset

import (
	"golang.org/x/exp/slices"

	workloads "github.com/apecloud/kubeblocks/apis/workloads/v1alpha1"
	"github.com/apecloud/kubeblocks/pkg/constant"
	"github.com/apecloud/kubeblocks/pkg/controller/rsm"
)

func mergeMap[K comparable, V any](src, dst *map[K]V) {
	if len(*src) == 0 {
		return
	}
	if *dst == nil {
		*dst = make(map[K]V)
	}
	for k, v := range *src {
		(*dst)[k] = v
	}
}

func mergeList[E any](src, dst *[]E, f func(E) func(E) bool) {
	if len(*src) == 0 {
		return
	}
	for i := range *src {
		item := (*src)[i]
		index := slices.IndexFunc(*dst, f(item))
		if index >= 0 {
			(*dst)[index] = item
		} else {
			*dst = append(*dst, item)
		}
	}
}

func getMatchLabels(name string) map[string]string {
	return map[string]string{
		rsm.WorkloadsManagedByLabelKey: managedBy,
		rsm.WorkloadsInstanceLabelKey:  name,
	}
}

func getSvcSelector(its *workloads.InstanceSet, headless bool) map[string]string {
	selectors := make(map[string]string)

	if !headless {
		for _, role := range its.Spec.Roles {
			if role.IsLeader && len(role.Name) > 0 {
				selectors[constant.RoleLabelKey] = role.Name
				break
			}
		}
	}

	for k, v := range its.Spec.Selector.MatchLabels {
		selectors[k] = v
	}
	return selectors
}
