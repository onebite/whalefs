package master

import (
	"net/http"
	"io/ioutil"
	"encoding/json"
	"time"
	"strings"
	"sync"
	"math/rand"
	"path/filepath"
	"fmt"
)

func (m *Master)publicEntry(w http.ResponseWriter, r *http.Request) {
	m.serverMutex.RLock()
	defer m.serverMutex.RUnlock()

	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		m.getFile(w, r)
	}else {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (m *Master)masterEntry(w http.ResponseWriter, r *http.Request) {
	m.serverMutex.RLock()
	defer m.serverMutex.RUnlock()

	switch r.URL.Path {
	case "/heartbeat":
		m.heartbeat(w, r)
	default:
		if r.URL.Path == "/favicon.ico" || len(r.URL.Path) <= 1 {
			http.NotFound(w, r)
			return
		}

		switch r.Method{
		case http.MethodGet, http.MethodHead:
			m.getFile(w, r)
		case http.MethodPost:
			m.uploadFile(w, r)
		case http.MethodDelete:
			m.deleteFile(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func (m *Master)heartbeat(w http.ResponseWriter, r *http.Request) {
	body, _ := ioutil.ReadAll(r.Body)
	newVms := new(VolumeManagerStatus)
	json.Unmarshal(body, newVms)
	newVms.LastHeartbeat = time.Now()

	remoteIP := r.RemoteAddr[:strings.LastIndex(r.RemoteAddr, ":")]
	if newVms.AdminHost == "" || newVms.AdminHost == "localhost" {
		newVms.AdminHost = remoteIP
	}
	if newVms.PublicHost == "" || newVms.PublicHost == "localhost" {
		newVms.PublicHost = remoteIP
	}
	if newVms.Machine == "" {
		newVms.Machine = remoteIP
	}

	m.statusMutex.Lock()
	defer m.statusMutex.Unlock()

	for i, oldVms := range m.VMStatusList {
		if oldVms.AdminHost == newVms.AdminHost && oldVms.AdminPort == newVms.AdminPort {
			m.VMStatusList = append(m.VMStatusList[:i], m.VMStatusList[i + 1:]...)
			for _, vs := range oldVms.VStatusList {
				vsList := m.VStatusListMap[vs.Id]
				if len(vsList) == 1 {
					delete(m.VStatusListMap, vs.Id)
					continue
				}
				for i, vs_ := range vsList {
					if vs == vs_ {
						vsList = append(vsList[:i], vsList[i + 1:]...)
						break
					}
				}
				m.VStatusListMap[vs.Id] = vsList
			}
			break
		}
	}

	needToCreateVolume := true

	m.VMStatusList = append(m.VMStatusList, newVms)

	for _, vs := range newVms.VStatusList {
		vs.vmStatus = newVms
		vsList := m.VStatusListMap[vs.Id]
		if vsList == nil {
			vsList = []*VolumeStatus{vs}
		}else {
			vsList = append(vsList, vs)
		}
		m.VStatusListMap[vs.Id] = vsList

		if needToCreateVolume && m.vStatusListIsValid(vsList) {
			needToCreateVolume = false
		}
	}

	if needToCreateVolume && newVms.canCreateVolume() {
		go m.createVolumeWithReplication(newVms)
	}
}

func (m *Master)getFile(w http.ResponseWriter, r *http.Request) {
	vid, fid, fileName, err := m.Metadata.Get(r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	m.statusMutex.RLock()
	vStatusList, ok := m.VStatusListMap[vid]
	m.statusMutex.RUnlock()
	if !ok {
		http.Error(w, "can't find volume", http.StatusNotFound)
		return
	}

	length := len(vStatusList)
	j := rand.Intn(length)
	for i := 0; i < length; i++ {
		vStatus := vStatusList[(i + j) % length]
		if vStatus.vmStatus.IsAlive() {
			http.Redirect(w, r, vStatus.getFileUrl(fid, fileName), http.StatusFound)
			return
		}
	}

	http.Error(w, "all volumes is dead", http.StatusInternalServerError)
}

func (m *Master)uploadFile(w http.ResponseWriter, r *http.Request) {
	//如果存在则删除旧文件,再上传新文件
	//vid, fid, fileName, err := m.Metadata.Get(r.URL.Path)
	//if err == nil {
	//	vStatusList := m.VStatusListMap[vid]
	//	vStatus := vStatusList[0]
	//	err = vStatus.delete(fid, fileName)
	//	if err != nil {
	//		http.Error(w, err.Error(), http.StatusInternalServerError)
	//		return
	//	}
	//	m.Metadata.Delete(r.URL.Path)
	//}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var dst string
	if r.URL.Path[len(r.URL.Path) - 1] == '/' {
		dst = r.URL.Path + filepath.Base(header.Filename)
	}else {
		dst = r.URL.Path
	}
	fileName := filepath.Base(dst)

	if m.Metadata.Has(dst) {
		http.Error(w, "file is existed, you should delete it at first.", http.StatusNotAcceptable)
		return
	}

	vStatusList, err := m.getWritableVolumes()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data, _ := ioutil.ReadAll(file)
	fid := m.generateFid()

	wg := sync.WaitGroup{}
	for _, vStatus := range vStatusList {
		wg.Add(1)
		go func(vs *VolumeStatus) {
			e := vs.uploadFile(fid, fileName, data)
			if e != nil {
				err = e
			}
			wg.Done()
		}(vStatus)
	}
	wg.Wait()

	if err != nil {
		for _, vStatus := range vStatusList {
			go vStatus.delete(fid, fileName)
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	m.Metadata.Set(dst, vStatusList[0].Id, fid, fileName)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (m *Master)deleteFile(w http.ResponseWriter, r *http.Request) {
	vid, fid, fileName, err := m.Metadata.Get(r.URL.Path)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	m.statusMutex.RLock()
	vStatusList, ok := m.VStatusListMap[vid]
	m.statusMutex.RUnlock()
	if !ok {
		http.Error(w, "can't find volume", http.StatusNotFound)
		return
	}else if !m.vStatusListIsValid(vStatusList) {
		http.Error(w, "can't delete file, because it's(volumes) readonly.", http.StatusNotAcceptable)
	}

	wg := sync.WaitGroup{}
	var deleteErr []error
	for _, vStatus := range vStatusList {
		wg.Add(1)
		go func(vStatus *VolumeStatus) {
			e := vStatus.delete(fid, fileName)
			if e != nil {
				deleteErr = append(deleteErr, fmt.Errorf("%s:%d %s", vStatus.vmStatus.AdminHost, vStatus.vmStatus.AdminPort, e.Error()))
			}
			wg.Done()
		}(vStatus)
	}
	wg.Wait()

	err = m.Metadata.Delete(r.URL.Path)
	if err != nil {
		deleteErr = append(deleteErr, fmt.Errorf("m.Metadata.Delete(%s) %s", r.URL.Path, err.Error()))
	}

	if len(deleteErr) == 0 {
		w.WriteHeader(http.StatusAccepted)
	}else {
		errStr := ""
		for _, err := range deleteErr {
			errStr += err.Error() + "\n"
		}
		http.Error(w, errStr, http.StatusInternalServerError)
		return
	}
}
