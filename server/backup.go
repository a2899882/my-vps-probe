package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"my-vps-probe/common"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func createHotBackupArchive() ([]byte, string, error) {
	now := time.Now()
	flushPingMinutes(now, true)
	flushMonthlyUsage()

	tmpDir, err := os.MkdirTemp("", "probe-hot-backup-*")
	if err != nil {
		return nil, "", fmt.Errorf("创建临时备份目录失败: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// 1. Snapshot config.json
	configMutex.RLock()
	cfgSnapshot := appConfig
	cfgSnapshot.Nodes = append([]common.NodeConfig(nil), appConfig.Nodes...)
	cfgSnapshot.GroupOrder = append([]string(nil), appConfig.GroupOrder...)
	cfgSnapshot.PingTasks = append([]common.PingTask(nil), appConfig.PingTasks...)
	configMutex.RUnlock()

	cfgBytes, err := os.ReadFile("config.json")
	if err != nil {
		cfgBytes, err = json.MarshalIndent(cfgSnapshot, "", "  ")
		if err != nil {
			return nil, "", fmt.Errorf("序列化配置失败: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), cfgBytes, 0600); err != nil {
		return nil, "", err
	}
	files := []string{"config.json", "data.db"}

	// 2. Snapshot data.db via VACUUM INTO (consistent online hot backup)
	dbSnapshotPath := filepath.Join(tmpDir, "data.db")
	if db != nil {
		_, vacErr := db.Exec(`VACUUM INTO ?`, dbSnapshotPath)
		if vacErr != nil {
			_, _ = db.Exec(`PRAGMA wal_checkpoint(PASSIVE)`)
			rawDB, readErr := os.ReadFile("data.db")
			if readErr != nil {
				return nil, "", fmt.Errorf("数据库快照失败: %v", vacErr)
			}
			if err := os.WriteFile(dbSnapshotPath, rawDB, 0600); err != nil {
				return nil, "", err
			}
		}
	} else {
		rawDB, err := os.ReadFile("data.db")
		if err != nil {
			return nil, "", fmt.Errorf("读取 data.db 失败: %w", err)
		}
		if err := os.WriteFile(dbSnapshotPath, rawDB, 0600); err != nil {
			return nil, "", err
		}
	}

	// 3. Snapshot usage_state.json if available
	monthlyUsageMutex.Lock()
	loadMonthlyUsageLocked()
	var usageBytes []byte
	if rawUsage, err := os.ReadFile(monthlyUsagePath); err == nil {
		usageBytes = rawUsage
	} else if len(monthlyUsageState) > 0 {
		usageBytes, _ = json.MarshalIndent(monthlyUsageState, "", "  ")
	}
	monthlyUsageMutex.Unlock()
	if len(usageBytes) > 0 {
		if err := os.WriteFile(filepath.Join(tmpDir, "usage_state.json"), usageBytes, 0600); err == nil {
			files = append(files, "usage_state.json")
		}
	}

	// 4. Write manifest.txt
	host, _ := os.Hostname()
	if host == "" {
		host = "probe-master"
	}
	manifest := fmt.Sprintf("format=my-vps-probe-backup-v1\ncreated_at=%s\nhostname=%s\n", now.Format(time.RFC3339), host)
	if err := os.WriteFile(filepath.Join(tmpDir, "manifest.txt"), []byte(manifest), 0600); err != nil {
		return nil, "", err
	}
	files = append(files, "manifest.txt")

	// 5. Generate SHA256SUMS compatible with `sha256sum -c`
	var sumBuf strings.Builder
	for _, name := range files {
		content, err := os.ReadFile(filepath.Join(tmpDir, name))
		if err != nil {
			return nil, "", err
		}
		hash := sha256.Sum256(content)
		sumBuf.WriteString(hex.EncodeToString(hash[:]))
		sumBuf.WriteString("  ")
		sumBuf.WriteString(name)
		sumBuf.WriteByte('\n')
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "SHA256SUMS"), []byte(sumBuf.String()), 0600); err != nil {
		return nil, "", err
	}
	allFiles := append(append([]string(nil), files...), "SHA256SUMS")

	// 6. Build .tar.gz in memory
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, name := range allFiles {
		fullPath := filepath.Join(tmpDir, name)
		info, err := os.Stat(fullPath)
		if err != nil {
			return nil, "", err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return nil, "", err
		}
		hdr.Name = name
		hdr.Mode = 0600
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, "", err
		}
		data, err := os.ReadFile(fullPath)
		if err != nil {
			return nil, "", err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, "", err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, "", err
	}
	if err := gz.Close(); err != nil {
		return nil, "", err
	}
	filename := fmt.Sprintf("my-vps-probe-backup-%s.tar.gz", now.Format("20060102-150405"))
	return out.Bytes(), filename, nil
}

func backupExportHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "仅支持 GET 或 POST")
		return
	}
	archive, filename, err := createHotBackupArchive()
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive)
}

func extractBackupPayload(r *http.Request) ([]byte, error) {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		_, params, err := mime.ParseMediaType(ct)
		if err != nil {
			return nil, errors.New("无效的上传请求格式")
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, errors.New("读取上传文件失败")
			}
			if part.FormName() == "file" || part.FileName() != "" {
				data, err := io.ReadAll(io.LimitReader(part, 128<<20))
				_ = part.Close()
				return data, err
			}
			_ = part.Close()
		}
		return nil, errors.New("未找到上传的备份文件")
	}
	return io.ReadAll(io.LimitReader(r.Body, 128<<20))
}

func verifyAndUnpackBackup(raw []byte, dstDir string) (hasDB bool, hasUsage bool, err error) {
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return false, false, errors.New("备份文件不是有效的 .tar.gz 压缩包")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	extracted := make(map[string][]byte)
	var totalBytes int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return false, false, errors.New("解压备份包失败")
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
		}
		cleanName := filepath.Base(strings.TrimPrefix(hdr.Name, "./"))
		if cleanName == "" || cleanName == "." || cleanName == ".." || strings.Contains(strings.TrimPrefix(hdr.Name, "./"), "/") {
			return false, false, fmt.Errorf("备份包包含非法文件路径: %s", hdr.Name)
		}
		switch cleanName {
		case "config.json", "data.db", "usage_state.json", "manifest.txt", "SHA256SUMS", "Caddyfile.probe":
		default:
			return false, false, fmt.Errorf("备份包包含未知文件: %s", cleanName)
		}
		data, err := io.ReadAll(io.LimitReader(tr, 128<<20))
		if err != nil {
			return false, false, errors.New("读取备份文件条目失败")
		}
		totalBytes += int64(len(data))
		if totalBytes > 128<<20 {
			return false, false, errors.New("备份包体积超过安全限制")
		}
		extracted[cleanName] = data
	}

	manifest, ok := extracted["manifest.txt"]
	if !ok || !strings.Contains(string(manifest), "format=my-vps-probe-backup-v1") {
		return false, false, errors.New("备份清单 manifest.txt 缺失或版本不匹配")
	}
	if _, ok := extracted["config.json"]; !ok {
		return false, false, errors.New("备份包缺少 config.json")
	}
	if sums, ok := extracted["SHA256SUMS"]; ok {
		for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return false, false, errors.New("SHA256SUMS 格式无效")
			}
			expectedHash := strings.ToLower(fields[0])
			fileName := filepath.Base(fields[len(fields)-1])
			content, exists := extracted[fileName]
			if !exists {
				return false, false, fmt.Errorf("校验文件缺失: %s", fileName)
			}
			actualHash := sha256.Sum256(content)
			if hex.EncodeToString(actualHash[:]) != expectedHash {
				return false, false, fmt.Errorf("文件 SHA256 校验失败: %s", fileName)
			}
		}
	}

	for name, content := range extracted {
		if err := os.WriteFile(filepath.Join(dstDir, name), content, 0600); err != nil {
			return false, false, err
		}
	}
	_, hasDB = extracted["data.db"]
	_, hasUsage = extracted["usage_state.json"]
	return hasDB, hasUsage, nil
}

func restoreDatabaseOnline(backupDBPath string) error {
	if db == nil {
		return writeFileAtomic("data.db", mustReadBytes(backupDBPath), 0600)
	}
	ensureNotificationColumns()
	ensureResourceHistoryColumns()
	conn, err := db.Conn(stringsContext())
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx := stringsContext()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS restore_src`, backupDBPath); err != nil {
		return fmt.Errorf("挂载备份数据库失败: %w", err)
	}
	defer func() {
		_, _ = conn.ExecContext(ctx, `DETACH DATABASE restore_src`)
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM main.ping_history`); err != nil {
		return err
	}
	_, _ = tx.ExecContext(ctx, `INSERT INTO main.ping_history(id,timestamp,server_id,target_name,delay,loss_rate) SELECT id,timestamp,server_id,target_name,delay,loss_rate FROM restore_src.ping_history`)

	if _, err := tx.ExecContext(ctx, `DELETE FROM main.resource_history`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO main.resource_history(id,timestamp,server_id,cpu_usage,mem_used,mem_total,disk_used,disk_total,swap_used,swap_total,load_1,net_in_speed,net_out_speed,tcp_connections,udp_connections) SELECT id,timestamp,server_id,cpu_usage,mem_used,mem_total,disk_used,disk_total,swap_used,swap_total,load_1,net_in_speed,net_out_speed,COALESCE(tcp_connections,0),COALESCE(udp_connections,0) FROM restore_src.resource_history`); err != nil {
		_, _ = tx.ExecContext(ctx, `INSERT INTO main.resource_history(id,timestamp,server_id,cpu_usage,mem_used,mem_total,disk_used,disk_total,swap_used,swap_total,load_1,net_in_speed,net_out_speed) SELECT id,timestamp,server_id,cpu_usage,mem_used,mem_total,disk_used,disk_total,swap_used,swap_total,load_1,net_in_speed,net_out_speed FROM restore_src.resource_history`)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM main.notification_events`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO main.notification_events(event_key,sent_at,sent_count) SELECT event_key,sent_at,COALESCE(sent_count,1) FROM restore_src.notification_events`); err != nil {
		_, _ = tx.ExecContext(ctx, `INSERT INTO main.notification_events(event_key,sent_at,sent_count) SELECT event_key,sent_at,1 FROM restore_src.notification_events`)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM main.notification_log`); err != nil {
		return err
	}
	_, _ = tx.ExecContext(ctx, `INSERT INTO main.notification_log(id,created_at,node_id,kind,status,message,error) SELECT id,created_at,node_id,kind,status,message,error FROM restore_src.notification_log`)

	return tx.Commit()
}

func mustReadBytes(path string) []byte {
	b, _ := os.ReadFile(path)
	return b
}

func backupRestoreHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeAPIError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<20)
	raw, err := extractBackupPayload(r)
	if err != nil || len(raw) == 0 {
		writeAPIError(w, http.StatusBadRequest, "读取备份文件失败")
		return
	}

	tmpDir, err := os.MkdirTemp("", "probe-restore-*")
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "创建临时恢复目录失败")
		return
	}
	defer os.RemoveAll(tmpDir)

	hasDB, hasUsage, err := verifyAndUnpackBackup(raw, tmpDir)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	cfgBytes, err := os.ReadFile(filepath.Join(tmpDir, "config.json"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "读取备份 config.json 失败")
		return
	}
	var newConfig AppConfig
	if err := json.Unmarshal(cfgBytes, &newConfig); err != nil {
		writeAPIError(w, http.StatusBadRequest, "备份 config.json 格式无效: "+err.Error())
		return
	}
	configMutex.RLock()
	if newConfig.AdminPass == "" {
		newConfig.AdminPass = appConfig.AdminPass
	}
	if newConfig.Telegram.Token == "" {
		newConfig.Telegram.Token = appConfig.Telegram.Token
	}
	configMutex.RUnlock()
	if err := validateConfig(&newConfig); err != nil {
		writeAPIError(w, http.StatusBadRequest, "备份配置校验未通过: "+err.Error())
		return
	}

	// Create automatic safety backup in backups/ before applying restore
	if safetyBytes, _, sErr := createHotBackupArchive(); sErr == nil {
		_ = os.MkdirAll("backups", 0700)
		safetyName := filepath.Join("backups", fmt.Sprintf("pre-restore-%s.tar.gz", time.Now().Format("20060102-150405")))
		_ = os.WriteFile(safetyName, safetyBytes, 0600)
	}

	if hasDB {
		if err := restoreDatabaseOnline(filepath.Join(tmpDir, "data.db")); err != nil {
			writeAPIError(w, http.StatusInternalServerError, "恢复历史数据库失败: "+err.Error())
			return
		}
	}

	monthlyUsageMutex.Lock()
	if hasUsage {
		usageBytes, err := os.ReadFile(filepath.Join(tmpDir, "usage_state.json"))
		if err == nil {
			_ = writeFileAtomic(monthlyUsagePath, usageBytes, 0600)
		}
	} else {
		_ = os.Remove(monthlyUsagePath)
	}
	monthlyUsageLoaded = false
	monthlyUsageDirty = false
	monthlyUsageLoadError = nil
	loadMonthlyUsageLocked()
	monthlyUsageMutex.Unlock()

	configMutex.Lock()
	renewNodes(newConfig.Nodes, time.Now())
	if err := saveAppConfig(newConfig); err != nil {
		configMutex.Unlock()
		writeAPIError(w, http.StatusInternalServerError, "写入恢复配置失败: "+err.Error())
		return
	}
	appConfig = newConfig
	configMutex.Unlock()

	invalidateCardPingCache()
	trendCache.Lock()
	trendCache.entries = make(map[string]trendCacheEntry)
	trendCache.Unlock()
	if events, err := loadNotificationEvents(); err == nil {
		resourceMonitor.restore(events)
		tcpBlockMonitor.restore(events)
	}
	hotPushAllAgents()

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"nodes":  len(newConfig.Nodes),
	})
}
