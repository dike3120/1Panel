package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/1Panel-dev/1Panel/backend/buserr"
	"github.com/1Panel-dev/1Panel/backend/utils/docker"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/1Panel-dev/1Panel/backend/app/dto"
	"github.com/1Panel-dev/1Panel/backend/app/dto/request"
	"github.com/1Panel-dev/1Panel/backend/app/dto/response"
	"github.com/1Panel-dev/1Panel/backend/app/model"
	"github.com/1Panel-dev/1Panel/backend/app/repo"
	"github.com/1Panel-dev/1Panel/backend/constant"
	"github.com/1Panel-dev/1Panel/backend/global"
	"github.com/1Panel-dev/1Panel/backend/utils/common"
	"github.com/1Panel-dev/1Panel/backend/utils/files"
	"gopkg.in/yaml.v3"
)

type AppService struct {
}

type IAppService interface {
	PageApp(req request.AppSearch) (interface{}, error)
	GetAppTags() ([]response.TagDTO, error)
	GetApp(key string) (*response.AppDTO, error)
	GetAppDetail(appId uint, version string) (response.AppDetailDTO, error)
	Install(ctx context.Context, req request.AppInstallCreate) (*model.AppInstall, error)
	SyncAppList() error
	GetAppUpdate() (*response.AppUpdateRes, error)
}

func NewIAppService() IAppService {
	return &AppService{}
}

func (a AppService) PageApp(req request.AppSearch) (interface{}, error) {
	var opts []repo.DBOption
	opts = append(opts, appRepo.OrderByRecommend())
	if req.Name != "" {
		opts = append(opts, commonRepo.WithLikeName(req.Name))
	}
	if req.Type != "" {
		opts = append(opts, appRepo.WithType(req.Type))
	}
	if req.Recommend {
		opts = append(opts, appRepo.GetRecommend())
	}
	if len(req.Tags) != 0 {
		tags, err := tagRepo.GetByKeys(req.Tags)
		if err != nil {
			return nil, err
		}
		var tagIds []uint
		for _, t := range tags {
			tagIds = append(tagIds, t.ID)
		}
		appTags, err := appTagRepo.GetByTagIds(tagIds)
		if err != nil {
			return nil, err
		}
		var appIds []uint
		for _, t := range appTags {
			appIds = append(appIds, t.AppId)
		}
		opts = append(opts, commonRepo.WithIdsIn(appIds))
	}
	var res response.AppRes
	total, apps, err := appRepo.Page(req.Page, req.PageSize, opts...)
	if err != nil {
		return nil, err
	}
	var appDTOs []*response.AppDTO
	for _, a := range apps {
		appDTO := &response.AppDTO{
			App: a,
		}
		appDTOs = append(appDTOs, appDTO)
		appTags, err := appTagRepo.GetByAppId(a.ID)
		if err != nil {
			continue
		}
		var tagIds []uint
		for _, at := range appTags {
			tagIds = append(tagIds, at.TagId)
		}
		tags, err := tagRepo.GetByIds(tagIds)
		if err != nil {
			continue
		}
		appDTO.Tags = tags
	}
	res.Items = appDTOs
	res.Total = total

	return res, nil
}

func (a AppService) GetAppTags() ([]response.TagDTO, error) {
	tags, err := tagRepo.All()
	if err != nil {
		return nil, err
	}
	var res []response.TagDTO
	for _, tag := range tags {
		res = append(res, response.TagDTO{
			Tag: tag,
		})
	}
	return res, nil
}

func (a AppService) GetApp(key string) (*response.AppDTO, error) {
	var appDTO response.AppDTO
	app, err := appRepo.GetFirst(appRepo.WithKey(key))
	if err != nil {
		return nil, err
	}
	appDTO.App = app
	details, err := appDetailRepo.GetBy(appDetailRepo.WithAppId(app.ID))
	if err != nil {
		return nil, err
	}
	var versionsRaw []string
	for _, detail := range details {
		versionsRaw = append(versionsRaw, detail.Version)
	}
	appDTO.Versions = common.GetSortedVersions(versionsRaw)

	return &appDTO, nil
}

func (a AppService) GetAppDetail(appId uint, version string) (response.AppDetailDTO, error) {
	var (
		appDetailDTO response.AppDetailDTO
		opts         []repo.DBOption
	)
	opts = append(opts, appDetailRepo.WithAppId(appId), appDetailRepo.WithVersion(version))
	detail, err := appDetailRepo.GetFirst(opts...)
	if err != nil {
		return appDetailDTO, err
	}
	paramMap := make(map[string]interface{})
	if err := json.Unmarshal([]byte(detail.Params), &paramMap); err != nil {
		return appDetailDTO, err
	}
	appDetailDTO.AppDetail = detail
	appDetailDTO.Params = paramMap
	appDetailDTO.Enable = true

	app, err := appRepo.GetFirst(commonRepo.WithByID(detail.AppId))
	if err != nil {
		return appDetailDTO, err
	}
	if err := checkLimit(app); err != nil {
		appDetailDTO.Enable = false
	}
	return appDetailDTO, nil
}

func (a AppService) Install(ctx context.Context, req request.AppInstallCreate) (*model.AppInstall, error) {
	if err := docker.CreateDefaultDockerNetwork(); err != nil {
		return nil, buserr.WithDetail(constant.Err1PanelNetworkFailed, err.Error(), nil)
	}
	if list, _ := appInstallRepo.ListBy(commonRepo.WithByName(req.Name)); len(list) > 0 {
		return nil, buserr.New(constant.ErrNameIsExist)
	}
	httpPort, err := checkPort("PANEL_APP_PORT_HTTP", req.Params)
	if err != nil {
		return nil, err
	}
	httpsPort, err := checkPort("PANEL_APP_PORT_HTTPS", req.Params)
	if err != nil {
		return nil, err
	}
	appDetail, err := appDetailRepo.GetFirst(commonRepo.WithByID(req.AppDetailId))
	if err != nil {
		return nil, err
	}
	app, err := appRepo.GetFirst(commonRepo.WithByID(appDetail.AppId))
	if err != nil {
		return nil, err
	}
	if err := checkRequiredAndLimit(app); err != nil {
		return nil, err
	}

	paramByte, err := json.Marshal(req.Params)
	if err != nil {
		return nil, err
	}
	appInstall := model.AppInstall{
		Name:        req.Name,
		AppId:       appDetail.AppId,
		AppDetailId: appDetail.ID,
		Version:     appDetail.Version,
		Status:      constant.Installing,
		Env:         string(paramByte),
		HttpPort:    httpPort,
		HttpsPort:   httpsPort,
		App:         app,
	}
	composeMap := make(map[string]interface{})
	if err := yaml.Unmarshal([]byte(appDetail.DockerCompose), &composeMap); err != nil {
		return nil, err
	}

	value, ok := composeMap["services"]
	if !ok {
		return nil, buserr.New("")
	}
	servicesMap := value.(map[string]interface{})
	changeKeys := make(map[string]string, len(servicesMap))
	index := 0
	for k := range servicesMap {
		serviceName := k + "-" + common.RandStr(4)
		changeKeys[k] = serviceName
		containerName := constant.ContainerPrefix + k + "-" + common.RandStr(4)
		if index > 0 {
			continue
		}
		req.Params["CONTAINER_NAME"] = containerName
		appInstall.ServiceName = serviceName
		appInstall.ContainerName = containerName
		index++
	}
	for k, v := range changeKeys {
		servicesMap[v] = servicesMap[k]
		delete(servicesMap, k)
	}
	composeByte, err := yaml.Marshal(composeMap)
	if err != nil {
		return nil, err
	}
	appInstall.DockerCompose = string(composeByte)

	if err := copyAppData(app.Key, appDetail.Version, req.Name, req.Params); err != nil {
		return nil, err
	}

	fileOp := files.NewFileOp()
	if err := fileOp.WriteFile(appInstall.GetComposePath(), strings.NewReader(string(composeByte)), 0775); err != nil {
		return nil, err
	}

	if err := appInstallRepo.Create(ctx, &appInstall); err != nil {
		return nil, err
	}
	if err := createLink(ctx, app, &appInstall, req.Params); err != nil {
		return nil, err
	}
	go upApp(appInstall.GetComposePath(), appInstall)
	go updateToolApp(appInstall)
	return &appInstall, nil
}

func (a AppService) GetAppUpdate() (*response.AppUpdateRes, error) {
	res := &response.AppUpdateRes{
		CanUpdate: false,
	}
	setting, err := NewISettingService().GetSettingInfo()
	if err != nil {
		return nil, err
	}
	versionUrl := fmt.Sprintf("%s/%s/%s/appstore/apps.json", global.CONF.System.RepoUrl, global.CONF.System.Mode, setting.SystemVersion)
	versionRes, err := http.Get(versionUrl)
	global.LOG.Infof("get current version from [%s]", versionUrl)
	if err != nil {
		return nil, err
	}
	defer versionRes.Body.Close()
	body, err := ioutil.ReadAll(versionRes.Body)
	if err != nil {
		return nil, err
	}
	list := &dto.AppList{}
	if err = json.Unmarshal(body, list); err != nil {
		return nil, err
	}
	res.Version = list.Version
	if setting.AppStoreVersion == "" || common.CompareVersion(list.Version, setting.AppStoreVersion) {
		res.CanUpdate = true
		res.DownloadPath = fmt.Sprintf("%s/%s/%s/appstore/apps-%s.tar.gz", global.CONF.System.RepoUrl, global.CONF.System.Mode, setting.SystemVersion, list.Version)
		return res, err
	}
	return res, nil
}

func (a AppService) SyncAppList() error {
	updateRes, err := a.GetAppUpdate()
	if err != nil {
		return err
	}
	if !updateRes.CanUpdate {
		global.LOG.Infof("The latest version is [%s] The app store is already up to date", updateRes.Version)
		return nil
	}
	if err := getAppFromRepo(updateRes.DownloadPath, updateRes.Version); err != nil {
		global.LOG.Errorf("get app from oss  error: %s", err.Error())
		return err
	}
	appDir := constant.AppResourceDir
	listFile := path.Join(appDir, "list.json")
	content, err := os.ReadFile(listFile)
	if err != nil {
		return err
	}
	list := &dto.AppList{}
	if err := json.Unmarshal(content, list); err != nil {
		return err
	}
	var (
		tags    []*model.Tag
		appTags []*model.AppTag
	)
	for _, t := range list.Tags {
		tags = append(tags, &model.Tag{
			Key:  t.Key,
			Name: t.Name,
		})
	}
	oldApps, err := appRepo.GetBy()
	if err != nil {
		return err
	}
	appsMap := getApps(oldApps, list.Items)
	for _, l := range list.Items {
		app := appsMap[l.Key]
		icon, err := os.ReadFile(path.Join(appDir, l.Key, "metadata", "logo.png"))
		if err != nil {
			global.LOG.Errorf("get [%s] icon error: %s", l.Name, err.Error())
			continue
		}
		iconStr := base64.StdEncoding.EncodeToString(icon)
		app.Icon = iconStr
		app.TagsKey = l.Tags
		if l.Recommend > 0 {
			app.Recommend = l.Recommend
		} else {
			app.Recommend = 9999
		}

		versions := l.Versions
		detailsMap := getAppDetails(app.Details, versions)

		for _, v := range versions {
			detail := detailsMap[v]
			detailPath := path.Join(appDir, l.Key, "versions", v)
			if _, err := os.Stat(detailPath); err != nil {
				global.LOG.Errorf("get [%s] folder error: %s", detailPath, err.Error())
				continue
			}
			readmeStr, err := os.ReadFile(path.Join(detailPath, "README.md"))
			if err != nil {
				global.LOG.Errorf("get [%s] README error: %s", detailPath, err.Error())
			}
			detail.Readme = string(readmeStr)
			dockerComposeStr, err := os.ReadFile(path.Join(detailPath, "docker-compose.yml"))
			if err != nil {
				global.LOG.Errorf("get [%s] docker-compose.yml error: %s", detailPath, err.Error())
				continue
			}
			detail.DockerCompose = string(dockerComposeStr)
			paramStr, err := os.ReadFile(path.Join(detailPath, "config.json"))
			if err != nil {
				global.LOG.Errorf("get [%s] form.json error: %s", detailPath, err.Error())
			}
			detail.Params = string(paramStr)
			detailsMap[v] = detail
		}
		var newDetails []model.AppDetail
		for _, v := range detailsMap {
			newDetails = append(newDetails, v)
		}
		app.Details = newDetails
		appsMap[l.Key] = app
	}

	var (
		addAppArray []model.App
		updateArray []model.App
	)
	tagMap := make(map[string]uint, len(tags))
	for _, v := range appsMap {
		if v.ID == 0 {
			addAppArray = append(addAppArray, v)
		} else {
			updateArray = append(updateArray, v)
		}
	}
	tx, ctx := getTxAndContext()
	if len(addAppArray) > 0 {
		if err := appRepo.BatchCreate(ctx, addAppArray); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tagRepo.DeleteAll(ctx); err != nil {
		tx.Rollback()
		return err
	}
	if len(tags) > 0 {
		if err := tagRepo.BatchCreate(ctx, tags); err != nil {
			tx.Rollback()
			return err
		}
		for _, t := range tags {
			tagMap[t.Key] = t.ID
		}
	}
	for _, update := range updateArray {
		if err := appRepo.Save(ctx, &update); err != nil {
			tx.Rollback()
			return err
		}
	}
	apps := append(addAppArray, updateArray...)

	var (
		addDetails    []model.AppDetail
		updateDetails []model.AppDetail
	)
	for _, a := range apps {
		for _, t := range a.TagsKey {
			tagId, ok := tagMap[t]
			if ok {
				appTags = append(appTags, &model.AppTag{
					AppId: a.ID,
					TagId: tagId,
				})
			}
		}
		for _, d := range a.Details {
			d.AppId = a.ID
			if d.ID == 0 {
				addDetails = append(addDetails, d)
			} else {
				updateDetails = append(updateDetails, d)
			}
		}
	}
	if len(addDetails) > 0 {
		if err := appDetailRepo.BatchCreate(ctx, addDetails); err != nil {
			tx.Rollback()
			return err
		}
	}
	for _, u := range updateDetails {
		if err := appDetailRepo.Update(ctx, u); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := appTagRepo.DeleteAll(ctx); err != nil {
		tx.Rollback()
		return err
	}
	if len(appTags) > 0 {
		if err := appTagRepo.BatchCreate(ctx, appTags); err != nil {
			tx.Rollback()
			return err
		}
	}
	tx.Commit()
	return nil
}
