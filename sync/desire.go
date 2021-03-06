package sync

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/baetyl/baetyl-go/v2/errors"
	"github.com/baetyl/baetyl-go/v2/log"
	specv1 "github.com/baetyl/baetyl-go/v2/spec/v1"
)

// extended features of config resourece
const (
	configKeyObject = "_object_"
	configValueZip  = "zip"
)

func (s *sync) SyncApps(infos []specv1.AppInfo) (map[string]specv1.Application, error) {
	appInfo := make(map[string]string)
	for _, info := range infos {
		appInfo[info.Name] = info.Version
	}
	crds, err := s.syncResourceValues(s.genResourceInfos(specv1.KindApplication, appInfo))
	if err != nil {
		s.log.Error("failed to sync application resource", log.Error(err))
		return nil, errors.Trace(err)
	}
	apps := make(map[string]specv1.Application)
	for _, r := range crds {
		if app := r.App(); app != nil {
			apps[app.Name] = *app
		}
	}
	return apps, nil
}

func (s *sync) SyncResource(info specv1.AppInfo) error {
	appInfo := map[string]string{info.Name: info.Version}
	crds, err := s.syncResourceValues(s.genResourceInfos(specv1.KindApplication, appInfo))
	if err != nil {
		s.log.Error("failed to sync application resource", log.Error(err))
		return errors.Trace(err)
	}
	cInfo := map[string]string{}
	sInfo := map[string]string{}
	apps := map[string]*specv1.Application{}
	for _, r := range crds {
		if app := r.App(); app != nil {
			apps[app.Name] = app
			for _, v := range app.Volumes {
				if v.Config != nil {
					cInfo[v.Config.Name] = v.Config.Version
				} else if v.Secret != nil {
					sInfo[v.Secret.Name] = v.Secret.Version
				}
			}
		} else {
			return errors.Errorf("failed to sync application (%s) (%s)", r.Name, r.Version)
		}
	}

	crds, err = s.syncResourceValues(s.genResourceInfos(specv1.KindConfiguration, cInfo))
	if err != nil {
		s.log.Error("failed to sync configuration resource", log.Error(err))
		return errors.Trace(err)
	}
	configs := map[string]*specv1.Configuration{}
	for _, r := range crds {
		if cfg := r.Config(); cfg != nil {
			configs[cfg.Name] = cfg
		} else {
			return errors.Errorf("failed to sync configuration (%s) (%s)", r.Name, r.Version)
		}
	}

	crds, err = s.syncResourceValues(s.genResourceInfos(specv1.KindSecret, sInfo))
	if err != nil {
		s.log.Error("failed to sync secret resource", log.Error(err))
		return errors.Trace(err)
	}
	secrets := map[string]*specv1.Secret{}
	for _, r := range crds {
		if secret := r.Secret(); secret != nil {
			secrets[secret.Name] = secret
		} else {
			return errors.Errorf("failed to sync secret (%s) (%s)", r.Name, r.Version)
		}
	}

	for _, app := range apps {
		err = s.processVolumes(app.Volumes, configs, secrets)
		if err != nil {
			s.log.Error("failed to process volumes", log.Error(err))
			return errors.Trace(err)
		}
		// app.volume may change when processing Volumes
		err := s.storeApplication(app)
		if err != nil {
			s.log.Error("failed to store application", log.Error(err))
			return errors.Trace(err)
		}
	}
	return nil
}

func (s *sync) syncResourceValues(crds []specv1.ResourceInfo) ([]specv1.ResourceValue, error) {
	if len(crds) == 0 {
		return nil, nil
	}
	msg := &specv1.Message{
		Kind:    specv1.MessageDesire,
		Content: specv1.DesireRequest{Infos: crds},
	}
	res, err := s.link.Request(msg)
	if err != nil {
		return nil, errors.Trace(err)
	}
	desire, ok := res.Content.(specv1.DesireResponse)
	if !ok {
		return nil, fmt.Errorf("unrecognized desrie response data")
	}
	return desire.Values, nil
}

func (s *sync) processVolumes(volumes []specv1.Volume, configs map[string]*specv1.Configuration, secrets map[string]*specv1.Secret) error {
	for i := range volumes {
		if cfg := volumes[i].VolumeSource.Config; cfg != nil && configs[cfg.Name] != nil {
			err := s.processConfiguration(&volumes[i], configs[cfg.Name])
			if err != nil {
				return errors.Trace(err)
			}
		} else if secret := volumes[i].VolumeSource.Secret; secret != nil && secrets[secret.Name] != nil {
			err := s.storeSecret(secrets[secret.Name])
			if err != nil {
				return errors.Trace(err)
			}
		}
	}
	return nil
}

func (s *sync) processConfiguration(volume *specv1.Volume, cfg *specv1.Configuration) error {
	var base, dir string
	for k, v := range cfg.Data {
		if strings.HasPrefix(k, configKeyObject) {
			if base == "" {
				base = filepath.Join(s.cfg.Download.Path, cfg.Name)
				dir = filepath.Join(base, cfg.Version)
				err := os.MkdirAll(dir, 0755)
				if err != nil {
					return errors.Trace(err)
				}
			}
			obj := new(specv1.ConfigurationObject)
			err := json.Unmarshal([]byte(v), &obj)
			if err != nil {
				s.log.Warn("process storage object of volume failed: %s", log.Any("name", volume.Name), log.Error(err))
				return errors.Trace(err)
			}
			filename := filepath.Join(dir, strings.TrimPrefix(k, configKeyObject))
			err = s.downloadObject(obj, dir, filename, obj.Unpack == configValueZip)
			if err != nil {
				os.RemoveAll(dir)
				return errors.Errorf("failed to download volume (%s) with error: %s", volume.Name, err)
			}
			if volume.HostPath == nil {
				err = cleanDir(base, cfg.Version)
				if err != nil {
					return errors.Trace(err)
				}
			}
		}
	}
	key := makeKey(specv1.KindConfiguration, cfg.Name, cfg.Version)
	if key == "" {
		return errors.Errorf("configuration does not have name or version")
	}
	if err := s.store.Get(key, &specv1.Configuration{}); err == nil {
		s.log.Info("configuration resource already exists", log.Any("key", key))
		return nil
	}
	return errors.Trace(s.store.Upsert(key, cfg))
}

func cleanDir(dir, retain string) error {
	files, _ := ioutil.ReadDir(dir)
	for _, f := range files {
		if f.Name() != retain {
			os.RemoveAll(filepath.Join(dir, f.Name()))
		}
	}
	return nil
}

func (s *sync) storeApplication(app *specv1.Application) error {
	key := makeKey(specv1.KindApplication, app.Name, app.Version)
	if key == "" {
		return errors.Errorf("app does not have name or version")
	}
	if err := s.store.Get(key, &specv1.Application{}); err == nil {
		s.log.Info("application resource already exists", log.Any("key", key))
		return nil
	}
	return errors.Trace(s.store.Upsert(key, app))
}

func (s *sync) storeSecret(secret *specv1.Secret) error {
	key := makeKey(specv1.KindSecret, secret.Name, secret.Version)
	if key == "" {
		return errors.Errorf("secret does not have name or version")
	}
	if err := s.store.Get(key, &specv1.Secret{}); err == nil {
		s.log.Info("secret resource already exists", log.Any("key", key))
		return nil
	}
	return errors.Trace(s.store.Upsert(key, secret))
}

func (s *sync) genResourceInfos(kind specv1.Kind, infos map[string]string) []specv1.ResourceInfo {
	var crds []specv1.ResourceInfo
	for name, version := range infos {
		crds = append(crds, specv1.ResourceInfo{
			Kind:    kind,
			Name:    name,
			Version: version,
		})
	}
	return crds
}

func makeKey(kind specv1.Kind, name, ver string) string {
	if name == "" || ver == "" {
		return ""
	}
	return string(kind) + "-" + name + "-" + ver
}
