/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.
 * Copyright (C) 2017-2018 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package logics

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"configcenter/src/common"
	"configcenter/src/common/auditlog"
	"configcenter/src/common/auth"
	"configcenter/src/common/backbone"
	"configcenter/src/common/blog"
	ccErr "configcenter/src/common/errors"
	"configcenter/src/common/http/rest"
	"configcenter/src/common/language"
	"configcenter/src/common/mapstr"
	"configcenter/src/common/metadata"
	"configcenter/src/common/util"
	"configcenter/src/framework/core/errors"
	hutil "configcenter/src/scene_server/host_server/util"
)

func (lgc *Logics) AddHost(kit *rest.Kit, appID int64, moduleIDs []int64, ownerID string, hostInfos map[int64]map[string]interface{}, importType metadata.HostInputType) ([]int64, []string, []string, []string, error) {
	if len(moduleIDs) == 0 {
		err := kit.CCError.CCErrorf(common.CCErrCommParamsInvalid, common.BKModuleIDField)
		return nil, nil, nil, nil, err
	}
	var err error
	defaultModule, err := lgc.CoreAPI.CoreService().Process().GetBusinessDefaultSetModuleInfo(kit.Ctx, kit.Header, appID)
	if err != nil {
		blog.Errorf("AddHost failed, get biz default module info failed, appID:%d, err:%s, rid:%s", appID, err.Error(), kit.Rid)
		return nil, nil, nil, nil, err
	}
	isInternalModule := make([]bool, 0)
	for _, moduleID := range moduleIDs {
		isInternalModule = append(isInternalModule, defaultModule.IsInternalModule(moduleID))
	}
	isInternalModule = util.BoolArrayUnique(isInternalModule)
	if len(isInternalModule) > 1 {
		err := kit.CCError.CCError(common.CCErrHostTransferFinalModuleConflict)
		return nil, nil, nil, nil, err
	}
	toInternalModule := isInternalModule[0]

	hostIDs := make([]int64, 0)
	instance := NewImportInstance(kit, ownerID, lgc)

	hostIDMap, err := instance.ExtractAlreadyExistHosts(kit.Ctx, hostInfos)
	if err != nil {
		blog.Errorf("get hosts failed, err:%s, rid:%s", err.Error(), kit.Rid)
		return nil, nil, nil, nil, err
	}

	var errMsg, updateErrMsg, successMsg []string
	logContents := make([]metadata.AuditLog, 0)
	auditHeaders, err := lgc.GetHostAttributes(kit, metadata.BizLabelNotExist)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	lang := util.GetLanguage(kit.Header)
	ccLang := lgc.Engine.Language.CreateDefaultCCLanguageIf(lang)
	iamInstances := make([]metadata.IamInstance, 0)
	for index, host := range hostInfos {
		if nil == host {
			continue
		}

		innerIP, isOk := host[common.BKHostInnerIPField].(string)
		if isOk == false || "" == innerIP {
			errMsg = append(errMsg, ccLang.Languagef("host_import_innerip_empty", index))
			continue
		}

		var iSubArea interface{}
		iSubArea, ok := host[common.BKCloudIDField]
		if false == ok {
			iSubArea = host[common.BKCloudIDField]
		}
		if nil == iSubArea {
			iSubArea = common.BKDefaultDirSubArea
		}

		iSubAreaVal, err := util.GetInt64ByInterface(iSubArea)
		if err != nil || iSubAreaVal < 0 {
			errMsg = append(errMsg, ccLang.Language("import_host_cloudID_invalid"))
			continue
		}
		host[common.BKCloudIDField] = iSubAreaVal

		var intHostID int64
		var existInDB bool

		// we support update host info both base on hostID and innerIP, hostID has higher priority then innerIP
		hostIDFromInput, bHostIDInInput := host[common.BKHostIDField]
		if bHostIDInInput == true {
			intHostID, err = util.GetInt64ByInterface(hostIDFromInput)
			if err != nil {
				errMsg = append(errMsg, ccLang.Language("import_host_hostID_not_int"))
				continue
			}
			existInDB = true
		} else {
			// try to get hostID from db
			key := generateHostCloudKey(innerIP, iSubAreaVal)
			intHostID, existInDB = hostIDMap[key]
		}
		var preData mapstr.MapStr
		var action metadata.ActionType
		// remove unchangeable fields
		delete(host, common.BKHostIDField)
		if existInDB {
			// remove unchangeable fields
			delete(host, common.BKHostInnerIPField)
			delete(host, common.BKCloudIDField)

			// get host info before really change it
			preData, _, _ = lgc.GetHostInstanceDetails(kit, intHostID)

			// update host instance.
			if err := instance.updateHostInstance(index, host, intHostID); err != nil {
				updateErrMsg = append(updateErrMsg, err.Error())
				continue
			}
			action = metadata.AuditUpdate
		} else {
			intHostID, err = instance.addHostInstance(iSubAreaVal, index, appID, moduleIDs, toInternalModule, host)
			if err != nil {
				errMsg = append(errMsg, fmt.Errorf(ccLang.Languagef("host_import_add_fail", index, innerIP, err.Error())).Error())
				continue
			}
			host[common.BKHostIDField] = intHostID
			hostIDMap[generateHostCloudKey(innerIP, iSubAreaVal)] = intHostID
			action = metadata.AuditCreate

			// record created host instance that would be registered to iam
			if auth.EnableAuthorize() {
				iamInstances = append(iamInstances, metadata.IamInstance{
					ID:   strconv.FormatInt(intHostID, 10),
					Name: innerIP,
				})
			}
		}
		// add current host operate result to  batch add result
		successMsg = append(successMsg, strconv.FormatInt(index, 10))

		// host info after it changed
		curData, _, err := lgc.GetHostInstanceDetails(kit, intHostID)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("generate audit log, but get host instance defail failed, err: %v", err)
		}

		bizName := ""
		if appID > 0 {
			bizName, err = auditlog.NewAudit(lgc.CoreAPI, kit.Header).GetInstNameByID(kit.Ctx, common.BKInnerObjIDApp, appID)
			if err != nil {
				return nil, nil, nil, nil, err
			}
		}

		// add audit log
		logContents = append(logContents, metadata.AuditLog{
			AuditType:    metadata.HostType,
			ResourceType: metadata.HostRes,
			Action:       action,
			OperationDetail: &metadata.InstanceOpDetail{
				BasicOpDetail: metadata.BasicOpDetail{
					BusinessID:   appID,
					BusinessName: bizName,
					ResourceID:   intHostID,
					ResourceName: innerIP,
					Details: &metadata.BasicContent{
						PreData:    preData,
						CurData:    curData,
						Properties: auditHeaders,
					},
				},
				ModelID: common.BKInnerObjIDHost,
			},
		})
		hostIDs = append(hostIDs, intHostID)
	}

	if len(logContents) > 0 {
		_, err := lgc.CoreAPI.CoreService().Audit().SaveAuditLog(context.Background(), kit.Header, logContents...)
		if err != nil {
			return hostIDs, successMsg, updateErrMsg, errMsg, fmt.Errorf("generate audit log, but get host instance defail failed, err: %v", err)
		}
	}

	if 0 < len(errMsg) || 0 < len(updateErrMsg) {
		return hostIDs, successMsg, updateErrMsg, errMsg, errors.New(ccLang.Language("host_import_err"))
	}

	return hostIDs, successMsg, updateErrMsg, errMsg, nil
}

func (lgc *Logics) AddHostToResourcePool(kit *rest.Kit, hostList metadata.AddHostToResourcePoolHostList) ([]int64, *metadata.AddHostToResourcePoolResult, error) {
	bizID, err := lgc.GetDefaultAppIDWithSupplier(kit)
	if err != nil {
		blog.ErrorJSON("add host, but get default biz id failed, err: %s, input: %s, rid: %s", err, hostList, kit.Rid)
		return nil, nil, err
	}

	var toInternalModule bool
	hostList.Directory, toInternalModule, err = lgc.GetModuleIDAndIsInternal(kit, bizID, hostList.Directory)
	if err != nil {
		return nil, nil, err
	}

	hostIDs := make([]int64, 0)
	res := new(metadata.AddHostToResourcePoolResult)
	instance := NewImportInstance(kit, kit.SupplierAccount, lgc)
	logContents := make([]metadata.AuditLog, 0)
	auditHeaders, err := lgc.GetHostAttributes(kit, nil)
	if err != nil {
		return nil, nil, err
	}

	iamInstances := make([]metadata.IamInstance, 0)

	for index, host := range hostList.HostInfo {
		if nil == host {
			continue
		}

		innerIP, exist := host[common.BKHostInnerIPField].(string)
		if !exist || "" == innerIP {
			res.Error = append(res.Error, metadata.AddOneHostToResourcePoolResult{
				Index:    index,
				ErrorMsg: kit.CCError.CCErrorf(common.CCErrCommParamsNeedSet, common.BKHostInnerIPField).Error(),
			})
			continue
		}
		cloudID, exist := host[common.BKCloudIDField]
		if !exist || cloudID == nil {
			res.Error = append(res.Error, metadata.AddOneHostToResourcePoolResult{
				Index:    index,
				ErrorMsg: kit.CCError.CCErrorf(common.CCErrCommParamsNeedSet, common.BKCloudIDField).Error(),
			})
			continue
		}

		// TODO remove this when bk_cloud_id field is upgraded to int type
		cloudIDVal, err := util.GetInt64ByInterface(cloudID)
		if err != nil || cloudIDVal < 0 {
			res.Error = append(res.Error, metadata.AddOneHostToResourcePoolResult{
				Index:    index,
				ErrorMsg: kit.CCError.CCErrorf(common.CCErrCommParamsNeedInt, common.BKCloudIDField).Error(),
			})
			continue
		}
		host[common.BKCloudIDField] = cloudIDVal

		hostID, err := instance.addHostInstance(cloudIDVal, int64(index), bizID, []int64{hostList.Directory},
			toInternalModule,
			host)
		if err != nil {
			res.Error = append(res.Error, metadata.AddOneHostToResourcePoolResult{
				Index:    index,
				ErrorMsg: err.Error(),
			})
			continue
		}
		host[common.BKHostIDField] = hostID

		hostIDs = append(hostIDs, hostID)
		res.Success = append(res.Success, metadata.AddOneHostToResourcePoolResult{
			Index:  index,
			HostID: hostID,
		})

		curData, _, err := lgc.GetHostInstanceDetails(kit, hostID)
		if err != nil {
			return hostIDs, res, fmt.Errorf("generate audit log, but get host instance defail failed, err: %v", err)
		}
		bizName := ""
		if bizID > 0 {
			bizName, err = auditlog.NewAudit(lgc.CoreAPI, kit.Header).GetInstNameByID(kit.Ctx, common.BKInnerObjIDApp, bizID)
			if err != nil {
				return hostIDs, res, err
			}
		}

		logContents = append(logContents, metadata.AuditLog{
			AuditType:    metadata.HostType,
			ResourceType: metadata.HostRes,
			Action:       metadata.AuditCreate,
			OperationDetail: &metadata.InstanceOpDetail{
				BasicOpDetail: metadata.BasicOpDetail{
					BusinessID:   bizID,
					BusinessName: bizName,
					ResourceID:   hostID,
					ResourceName: host[common.BKHostInnerIPField].(string),
					Details: &metadata.BasicContent{
						PreData:    nil,
						CurData:    curData,
						Properties: auditHeaders,
					},
				},
				ModelID: common.BKInnerObjIDHost,
			},
		})

		// record created host instance that would be registered to iam
		if auth.EnableAuthorize() {
			iamInstances = append(iamInstances, metadata.IamInstance{
				ID:   strconv.FormatInt(hostID, 10),
				Name: innerIP,
			})
		}
	}

	if len(logContents) > 0 {
		_, err := lgc.CoreAPI.CoreService().Audit().SaveAuditLog(context.Background(), kit.Header, logContents...)
		if err != nil {
			return hostIDs, res, fmt.Errorf("generate audit log, but get host instance defail failed, err: %v", err)
		}
	}

	if 0 < len(res.Error) {
		return hostIDs, res, kit.CCError.CCErrorf(common.CCErrHostCreateFail)
	}

	return hostIDs, res, nil
}

func (lgc *Logics) getHostFields(kit *rest.Kit) (map[string]*metadata.ObjAttDes, error) {
	opt := hutil.NewOperation().WithObjID(common.BKInnerObjIDHost).MapStr()

	input := &metadata.QueryCondition{
		Condition: opt,
	}
	result, err := lgc.CoreAPI.CoreService().Model().
		ReadModelAttr(kit.Ctx, kit.Header, common.BKInnerObjIDHost, input)
	if err != nil {
		blog.Errorf("getHostFields http do error, err:%s, input:%+v, rid:%s", err.Error(), input, kit.Rid)
		return nil, kit.CCError.Error(common.CCErrCommHTTPDoRequestFailed)
	}
	if !result.Result {
		blog.Errorf("getHostFields http response error, err code:%d, err msg:%s, input:%+v, rid:%s", result.Code, result.ErrMsg, input, kit.Rid)
		return nil, kit.CCError.New(result.Code, result.ErrMsg)
	}

	attributesDesc := make([]metadata.ObjAttDes, 0)
	for _, att := range result.Data.Info {
		attributesDesc = append(attributesDesc, metadata.ObjAttDes{Attribute: att})
	}

	fields := make(map[string]*metadata.ObjAttDes)
	for index, f := range attributesDesc {
		fields[f.PropertyID] = &attributesDesc[index]
	}
	return fields, nil
}

// generateHostCloudKey generate a cloudKey for host that is unique among clouds by appending the cloudID.
func generateHostCloudKey(ip, cloudID interface{}) string {
	return fmt.Sprintf("%v-%v", ip, cloudID)
}

type importInstance struct {
	*backbone.Engine
	pheader   http.Header
	inputType metadata.HostInputType
	ownerID   string
	// cloudID       int64
	// hostInfos     map[int64]map[string]interface{}
	defaultFields map[string]*metadata.ObjAttDes
	rowErr        map[int64]error
	ctx           context.Context
	ccErr         ccErr.DefaultCCErrorIf
	ccLang        language.DefaultCCLanguageIf
	rid           string
	lgc           *Logics
	kit           *rest.Kit
}

func NewImportInstance(kit *rest.Kit, ownerID string, lgc *Logics) *importInstance {
	lang := util.GetLanguage(kit.Header)
	return &importInstance{
		pheader: kit.Header,
		Engine:  lgc.Engine,
		ownerID: ownerID,
		ctx:     kit.Ctx,
		ccErr:   kit.CCError,
		ccLang:  lgc.Engine.Language.CreateDefaultCCLanguageIf(lang),
		rid:     kit.Rid,
		lgc:     lgc,
		kit:     kit,
	}
}

func (h *importInstance) updateHostInstance(index int64, host map[string]interface{}, hostID int64) error {
	delete(host, "import_from")
	delete(host, common.CreateTimeField)

	// 更新主机数据
	input := &metadata.UpdateOption{}
	input.Condition = map[string]interface{}{common.BKHostIDField: hostID}
	input.Data = host
	uResult, err := h.CoreAPI.CoreService().Instance().UpdateInstance(h.ctx, h.pheader, common.BKInnerObjIDHost, input)
	if err != nil {
		ip, _ := host[common.BKHostInnerIPField].(string)
		blog.Errorf("updateHostInstance http do error,  err:%s,input:%+v,rid:%s", err.Error(), input, h.rid)
		return fmt.Errorf(h.ccLang.Languagef("host_import_update_fail", index, ip, err.Error()))
	}
	if !uResult.Result {
		ip, _ := host[common.BKHostInnerIPField].(string)
		blog.Errorf("updateHostInstance http response error,  err code:%d, err msg:%s,input:%+v,rid:%s", uResult.Code, uResult.ErrMsg, input, h.rid)
		return fmt.Errorf(h.ccLang.Languagef("host_import_update_fail", index, ip, uResult.ErrMsg))
	}
	return nil
}

// addHostInstance  add host
// cloud id：host belong cloud area id
// index: index number
// app id : host belong app id
// module id: host belong module id
// host : host info
func (h *importInstance) addHostInstance(cloudID, index, appID int64, moduleIDs []int64, toInternalModule bool, host map[string]interface{}) (int64, error) {
	ip, _ := host[common.BKHostInnerIPField].(string)
	if cloudID < 0 {
		return 0, fmt.Errorf(h.ccLang.Languagef("host_import_add_fail", index, ip, h.ccLang.Language("import_host_cloudID_invalid")))
	}

	// determine if the cloud area exists
	// default cloud area must be exist
	if cloudID != common.BKDefaultDirSubArea {
		isExist, err := h.lgc.IsPlatExist(h.kit, mapstr.MapStr{common.BKCloudIDField: cloudID})
		if nil != err {
			return 0, fmt.Errorf(h.ccLang.Languagef("host_import_add_fail", index, ip, err.Error()))

		}
		if !isExist {
			return 0, fmt.Errorf(h.ccLang.Languagef("host_import_add_fail", index, ip, h.ccErr.Errorf(common.CCErrTopoCloudNotFound).Error()))

		}
	}
	host[common.BKCloudIDField] = cloudID

	input := &metadata.CreateModelInstance{
		Data: host,
	}

	// (h.ctx, h.pheader, host)
	var err error
	result, err := h.CoreAPI.CoreService().Instance().CreateInstance(h.ctx, h.pheader, common.BKInnerObjIDHost, input)
	if err != nil {
		blog.Errorf("addHostInstance http do error,err:%s, input:%+v,rid:%s", err.Error(), host, h.rid)
		return 0, err
	}
	if !result.Result {
		blog.Errorf("addHostInstance http response error,err code:%d,err msg:%s, input:%+v,rid:%s", result.Code, result.ErrMsg, host, h.rid)
		return 0, result.CCError()
	}

	hostID := int64(result.Data.Created.ID)
	var hResult *metadata.OperaterException
	var option interface{}
	if toInternalModule == true {
		if len(moduleIDs) == 0 {
			err := h.ccErr.CCErrorf(common.CCErrCommParamsInvalid, common.BKModuleIDField)
			return 0, err
		}
		opt := &metadata.TransferHostToInnerModule{
			ApplicationID: appID,
			ModuleID:      moduleIDs[0],
			HostID:        []int64{hostID},
		}
		option = opt
		hResult, err = h.CoreAPI.CoreService().Host().TransferToInnerModule(h.ctx, h.pheader, opt)
	} else {
		opt := &metadata.HostsModuleRelation{
			ApplicationID: appID,
			ModuleID:      moduleIDs,
			HostID:        []int64{hostID},
		}
		option = opt
		hResult, err = h.CoreAPI.CoreService().Host().TransferToNormalModule(h.ctx, h.pheader, opt)

	}
	if err != nil {
		blog.Errorf("add host module by ip:%s  err:%s,input:%+v,rid:%s", ip, err.Error(), option, h.rid)
		return 0, err
	}
	if !hResult.Result {
		blog.Errorf("add host module by ip:%s , result:%#v, input:%#v, rid:%s", ip, hResult.Code, option, h.rid)
		if len(hResult.Data) > 0 {
			return 0, h.ccErr.New(int(hResult.Data[0].Code), hResult.Data[0].Message)
		}
		return 0, hResult.CCError()
	}

	return hostID, nil
}

// ExtractAlreadyExistHosts extract hosts that already in db(same innerIP host)
// return: map[hostKey]hostID
func (h *importInstance) ExtractAlreadyExistHosts(ctx context.Context, hostInfos map[int64]map[string]interface{}) (map[string]int64, error) {
	// step1. extract all innerIP from hostInfos
	var ipArr []string
	for _, host := range hostInfos {
		innerIP, isOk := host[common.BKHostInnerIPField].(string)
		if isOk && "" != innerIP {
			ipArr = append(ipArr, innerIP)
		}
	}
	if len(ipArr) == 0 {
		return make(map[string]int64), nil
	}

	// step2. query host info by innerIPs
	ipCond := make([]map[string]interface{}, len(ipArr))
	for index, innerIP := range ipArr {
		innerIPArr := strings.Split(innerIP, ",")
		ipCond[index] = map[string]interface{}{
			common.BKHostInnerIPField: map[string]interface{}{
				common.BKDBAll:  innerIPArr,
				common.BKDBSize: len(innerIPArr),
			},
		}
	}
	filter := map[string]interface{}{
		common.BKDBOR: ipCond,
	}
	query := &metadata.QueryCondition{
		Condition: filter,
		Page: metadata.BasePage{
			Start: 0,
			Limit: common.BKNoLimit,
		},
	}
	hResult, err := h.CoreAPI.CoreService().Instance().ReadInstance(ctx, h.pheader, common.BKInnerObjIDHost, query)
	if err != nil {
		blog.Errorf("GetHostIDByHostInfoArr ReadInstance http do err. error:%s, input:%#v, rid:%s", err.Error(), query, h.rid)
		return nil, h.ccErr.CCError(common.CCErrCommHTTPDoRequestFailed)
	}
	if !hResult.Result {
		blog.Errorf("GetHostIDByHostInfoArr ReadInstance http reply err. reply:%#v, input:%#v, rid:%s", hResult, query, h.rid)
		return nil, h.ccErr.New(hResult.Code, hResult.ErrMsg)
	}

	// step3. arrange data as a map, cloudKey: hostID
	hostMap := make(map[string]int64, 0)
	for _, host := range hResult.Data.Info {
		key := generateHostCloudKey(host[common.BKHostInnerIPField], host[common.BKCloudIDField])
		hostID, err := host.Int64(common.BKHostIDField)
		if err != nil {
			blog.Errorf("GetHostIDByHostInfoArr get hostID error. err:%s, hostInfo:%#v, rid:%s", err.Error(), host, h.rid)
			// message format: `convert %s  field %s to %s error %s`
			return hostMap, h.ccErr.Errorf(common.CCErrCommInstFieldConvertFail, common.BKInnerObjIDHost, common.BKHostIDField, "int", err.Error())
		}
		hostMap[key] = hostID
	}

	return hostMap, nil
}
