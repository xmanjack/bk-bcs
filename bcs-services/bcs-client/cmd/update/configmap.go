/*
 * Tencent is pleased to support the open source community by making Blueking Container Service available.
 * Copyright (C) 2019 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package update

import (
	"fmt"

	"bk-bcs/bcs-services/bcs-client/cmd/utils"
	"bk-bcs/bcs-services/bcs-client/pkg/scheduler/v4"
)

func updateConfigMap(c *utils.ClientContext) error {
	if err := c.MustSpecified(utils.OptionClusterID); err != nil {
		return err
	}

	data, err := c.FileData()
	if err != nil {
		return err
	}

	namespace, err := utils.ParseNamespaceFromJson(data)
	if err != nil {
		return err
	}

	scheduler := v4.NewBcsScheduler(utils.GetClientOption())
	err = scheduler.UpdateConfigMap(c.ClusterID(), namespace, data, nil)
	if err != nil {
		return fmt.Errorf("failed to update configmap: %v", err)
	}

	fmt.Printf("success to update configmap\n")
	return nil
}
