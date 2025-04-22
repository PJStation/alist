package alistlib

import (
	"context"
	"errors"
	"fmt"
	"github.com/alist-org/alist/v3/cmd"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/alist-org/alist/v3/alistlib/internal"
	"github.com/alist-org/alist/v3/cmd/flags"
	"github.com/alist-org/alist/v3/internal/bootstrap"
	"github.com/alist-org/alist/v3/internal/conf"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/alist-org/alist/v3/server"
	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

type LogCallback interface {
	OnLog(level int16, time int64, message string)
}

type Event interface {
	OnStartError(t string, err string)
	OnShutdown(t string)
	OnProcessExit(code int)
}

var event Event
var logFormatter *internal.MyFormatter

func Init(e Event, cb LogCallback) error {
	event = e
	logFormatter = &internal.MyFormatter{
		OnLog: func(entry *log.Entry) {
			cb.OnLog(int16(entry.Level), entry.Time.UnixMilli(), entry.Message)
		},
	}
	if utils.Log == nil {
		return errors.New("utils.log is nil")
	} else {
		utils.Log.SetFormatter(logFormatter)
		utils.Log.ExitFunc = event.OnProcessExit
	}
	checkConfigJSON()
	cmd.Init()
	return nil
}

var httpSrv, httpsSrv, unixSrv *http.Server

func listenAndServe(t string, srv *http.Server) {
	err := srv.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		event.OnStartError(t, err.Error())
	} else {
		event.OnShutdown(t)
	}
}

func IsRunning(t string) bool {
	switch t {
	case "http":
		return httpSrv != nil
	case "https":
		return httpsSrv != nil
	case "unix":
		return unixSrv != nil
	}

	return httpSrv != nil && httpsSrv != nil && unixSrv != nil
}

// Start starts the server
func Start() {
	if conf.Conf.DelayedStart != 0 {
		utils.Log.Infof("delayed start for %d seconds", conf.Conf.DelayedStart)
		time.Sleep(time.Duration(conf.Conf.DelayedStart) * time.Second)
	}
	bootstrap.InitOfflineDownloadTools()
	bootstrap.LoadStorages()
	bootstrap.InitTaskManager()
	if !flags.Debug && !flags.Dev {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	r.Use(gin.LoggerWithWriter(log.StandardLogger().Out), gin.RecoveryWithWriter(log.StandardLogger().Out))
	server.Init(r)

	if conf.Conf.Scheme.HttpPort != -1 {
		httpBase := fmt.Sprintf("%s:%d", conf.Conf.Scheme.Address, conf.Conf.Scheme.HttpPort)
		utils.Log.Infof("start HTTP server @ %s", httpBase)
		httpSrv = &http.Server{Addr: httpBase, Handler: r}
		go func() {
			listenAndServe("http", httpSrv)
			httpSrv = nil
		}()
	}
	if conf.Conf.Scheme.HttpsPort != -1 {
		httpsBase := fmt.Sprintf("%s:%d", conf.Conf.Scheme.Address, conf.Conf.Scheme.HttpsPort)
		utils.Log.Infof("start HTTPS server @ %s", httpsBase)
		httpsSrv = &http.Server{Addr: httpsBase, Handler: r}
		go func() {
			listenAndServe("https", httpsSrv)
			httpsSrv = nil
		}()
	}
	if conf.Conf.Scheme.UnixFile != "" {
		utils.Log.Infof("start unix server @ %s", conf.Conf.Scheme.UnixFile)
		unixSrv = &http.Server{Handler: r}
		go func() {
			listener, err := net.Listen("unix", conf.Conf.Scheme.UnixFile)
			if err != nil {
				//utils.Log.Fatalf("failed to listenAndServe unix: %+v", err)
				event.OnStartError("unix", err.Error())
			} else {
				// set socket file permission
				mode, err := strconv.ParseUint(conf.Conf.Scheme.UnixFilePerm, 8, 32)
				if err != nil {
					utils.Log.Errorf("failed to parse socket file permission: %+v", err)
				} else {
					err = os.Chmod(conf.Conf.Scheme.UnixFile, os.FileMode(mode))
					if err != nil {
						utils.Log.Errorf("failed to chmod socket file: %+v", err)
					}
				}
				err = unixSrv.Serve(listener)
				if err != nil && err != http.ErrServerClosed {
					event.OnStartError("unix", err.Error())
				}
			}

			unixSrv = nil
		}()
	}
}

func shutdown(srv *http.Server, timeout time.Duration) error {
	if srv == nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := srv.Shutdown(ctx)

	return err
}

// Shutdown timeout 毫秒
func Shutdown(timeout int64) (err error) {
	timeoutDuration := time.Duration(timeout) * time.Millisecond
	utils.Log.Println("Shutdown server...")
	if conf.Conf.Scheme.HttpPort != -1 {
		err := shutdown(httpSrv, timeoutDuration)
		if err != nil {
			return err
		}
		httpSrv = nil
		utils.Log.Println("Server HTTP Shutdown")
	}
	if conf.Conf.Scheme.HttpsPort != -1 {
		err := shutdown(httpsSrv, timeoutDuration)
		if err != nil {
			return err
		}
		httpsSrv = nil
		utils.Log.Println("Server HTTPS Shutdown")
	}
	if conf.Conf.Scheme.UnixFile != "" {
		err := shutdown(unixSrv, timeoutDuration)
		if err != nil {
			return err
		}
		unixSrv = nil
		utils.Log.Println("Server UNIX Shutdown")
	}

	//cmd.Release()
	return nil
}
func checkConfigJSON() {
	configPath := filepath.Join(flags.DataDir, "config.json")
	// 检查 config.json 文件是否存在
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		utils.Log.Println("config.json does not exist in the provided path.")
		return
	}

	// 读取 config.json 文件内容
	content, err := os.ReadFile(configPath)
	if err != nil {
		utils.Log.Println("Failed to read config.json: %v\n", err)
		return
	}
	contentStr := string(content)

	// 检查文件中是否已是正确的路径
	if strings.Contains(contentStr, flags.DataDir) {
		utils.Log.Println("config.json already includes the correct path, no further actions taken.")
		return
	}

	// 创建用于查找匹配路径模式的正则表达式
	patternString := regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`).ReplaceAllString(flags.DataDir, `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	regexPattern, err := regexp.Compile(patternString)
	if err != nil {
		utils.Log.Println("Error compiling regex:", err)
		return
	}

	// 替换 config.json 中的匹配内容为正确的路径
	updatedContent := regexPattern.ReplaceAllString(contentStr, flags.DataDir)

	// 将更新后的内容写回 config.json
	if err = os.WriteFile(configPath, []byte(updatedContent), 0755); err != nil {
		utils.Log.Println("Failed to write updated config.json: %v\n", err)
		return
	}
	utils.Log.Println("Config.json has been successfully updated.")
}
