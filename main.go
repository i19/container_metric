package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
)

const (
	CpuUserHz         = 100
	CpuMetricInterval = 5
	Step              = 10
	CGroupDir         = "/cgroup"
	PidFile           = "/persist/logs/app.pid"
	LogFile           = "/var/log/collect_container_metric.log"
)

var (
	MetricChannel = make(chan Metric, 10000)
	Prefix        = "container."
)

type Metric struct {
	Name        string      `json:"metric"`
	Value       interface{} `json:"value"`
	CounterType string      `json:"counterType"`
	Timestamp   string      `json:"Timestamp"`
}

func mewMetric() Metric {
	metric := Metric{}
	metric.CounterType = "GAUGE"
	metric.Timestamp = strconv.FormatInt(time.Now().UnixNano()/int64(time.Second), 10)
	return metric
}

func MetricCpu(myId string, metric Metric) {
	userSystemCPUFile := fmt.Sprintf("%s/cpuacct/docker/%s/cpuacct.stat", CGroupDir, myId)
	cpuUsageFile := fmt.Sprintf("%s/cpuacct/docker/%s/cpuacct.usage", CGroupDir, myId)

	lastCPU, _ := fileToMap(userSystemCPUFile)
	lastCPUUsage, _ := getLine(cpuUsageFile)
	time.Sleep(time.Second * CpuMetricInterval)
	currentCPU, _ := fileToMap(userSystemCPUFile)
	currentCPUUsage, _ := getLine(cpuUsageFile)

	if cUser, cuOK := currentCPU["user"]; cuOK {
		if lUser, luOk := lastCPU["user"]; luOk {
			userValue := float64(cUser-lUser) / float64(CpuMetricInterval*CpuUserHz)
			MetricToChannel(metric, "cpu.user", userValue)
		} else {
			logrus.Error("error: get lastCPU[user] failed")
		}
	} else {
		logrus.Error("error: get current[user] failed")
	}

	if cSystem, csOk := currentCPU["system"]; csOk {
		if lSystem, lsOk := lastCPU["system"]; lsOk {
			systemValue := float64(cSystem-lSystem) / float64(CpuMetricInterval*CpuUserHz)
			MetricToChannel(metric, "cpu.system", systemValue)
		} else {
			logrus.Error("error: get lastCPU[system] failed")
		}
	} else {
		logrus.Error("error: get current[system] failed")
	}

	totalValue := float64(currentCPUUsage.(int64)-lastCPUUsage.(int64)) / float64(CpuMetricInterval*1000000000)

	MetricToChannel(metric, "cpu.total", totalValue)
}

func MetricMemory(myId string, metric Metric) {
	metricFile := fmt.Sprintf("%s/memory/docker/%s/memory.stat", CGroupDir, myId)
	memory, _ := fileToMap(metricFile)

	totalCacheValue, cacheOk := memory["total_cache"]
	totalRssValue, rssOk := memory["total_rss"]

	if cacheOk {
		MetricToChannel(metric, "mem.cached", totalCacheValue)
	} else {
		logrus.Error("error: get memory[total_cache]")
	}

	if rssOk {
		MetricToChannel(metric, "mem.rss", totalRssValue)
	} else {
		logrus.Error("error: get memory[total_rss]")
	}

	if cacheOk && rssOk {
		MetricToChannel(metric, "mem.total", totalCacheValue+totalRssValue)
	} else {
		logrus.Error("error: get memory[total_cache] or memory[total_rss]")
	}
}

func MetricConnection(metric Metric) {
	command := fmt.Sprint("netstat  -atnp|sed '1,2d'|awk '{A[$6]+=1}END{for(i in A) print i,A[i]}'")
	out, executeErr := exec.Command("/bin/bash", "-c", command).Output()
	if executeErr != nil {
		logrus.Error("error:MetricConnection execute error: ", executeErr)
	}
	connectionList := strings.Split(string(out), "\n")
	for _, item := range connectionList {
		kv := strings.Fields(item)
		if len(kv) == 2 {
			metricName := strings.ToLower("netstate." + string(kv[0]))
			metricValue, convertErr := strconv.ParseInt(string(kv[1]), 10, 64)
			if convertErr != nil {
				logrus.Error("error: MetricConnection: converting error ", convertErr, kv)
			} else {
				MetricToChannel(metric, metricName, metricValue)
			}
		}
	}
}

func MetricProgress(metric Metric) {
	value := int64(0)
	pid, getLineErr := getLine(PidFile)
	if getLineErr != nil {
		MetricToChannel(metric, "app.alive", value)
		return
	}
	pidType := reflect.TypeOf(pid).String()
	if pidType == "int64" {
		process, err := os.FindProcess(int(pid.(int64)))
		if err != nil {
			fmt.Printf("Failed to find process: %s\n", err)
			value = 0
		} else {
			err := process.Signal(syscall.Signal(0))
			if err == nil {
				value = 1
			} else {
				value = 0
			}
		}
	} else {
		logrus.Error("Find pid failed, pid should be int64")
		value = 0
	}
	MetricToChannel(metric, "app.alive", value)
}

func MetricAgentAlive(metric Metric) {
	MetricToChannel(metric, "metric_agent.alive", int64(1))
}

func MetricPort(metric Metric) {
	pid, err := getLine(PidFile)
	if err != nil {
		logrus.Error("MetricPort Error: ", err)
	}
	command := fmt.Sprintf("ss -ltn4p|grep ',%d,'|cut -d':' -f2|awk '{print $1}'", pid)
	out, executeErr := exec.Command("/bin/bash", "-c", command).Output()
	if executeErr != nil {
		logrus.Error("error:MetricPort execute error: ", executeErr)
		return
	}
	portList := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, port := range portList {
		metricName := fmt.Sprintf("port.%s", port)
		MetricToChannel(metric, metricName, int64(1))
	}
}

func MetricNetPacket(metric Metric) {
	metricItem := metric
	metricItem.CounterType = "COUNTER"
	metricList := []string{"rx.bytes", "rx.packets", "rx.errs", "rx.drop", "rx.fifo", "rx.frame", "rx.compressed", "rx.multicast",
		"tx.bytes", "tx.packets", "tx.errs", "tx.drop", "tx.fifo", "tx.colls", "tx.carrier", "tx.compressed"}
	filePath := "/proc/net/dev"
	file, openFileErr := os.Open(filePath)
	defer file.Close()
	if openFileErr != nil {
		logrus.Error("error: MetricNetPacket: open file failed: ", filePath, openFileErr)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		lineSlice := strings.Fields(line)
		metricObject := lineSlice[0]
		metricObject = strings.TrimRight(metricObject, ":")
		lineSlice = lineSlice[1:]
		if strings.HasPrefix(metricObject, "eth") || strings.HasPrefix(metricObject, "cali") {
			for index, metricName := range metricList {
				MetricToChannel(metricItem, "traffic."+metricObject+"."+metricName, lineSlice[index])
			}
		}
	}
}

func MetricBlkIO(myId string, metric Metric) {
	metricItem := metric
	metricItem.CounterType = "COUNTER"
	metricFiles := map[string]string{}
	metricFiles["blkio.iops"] = fmt.Sprintf("%s/blkio/docker/%s/blkio.throttle.io_serviced", CGroupDir, myId)
	metricFiles["blkio.bytes"] = fmt.Sprintf("%s/blkio/docker/%s/blkio.throttle.io_service_bytes", CGroupDir, myId)
	for metricPrefix, metricFile := range metricFiles {
		go func(metric Metric, metricPrefix string, metricFile string) {
			file, openFileErr := os.Open(metricFile)
			defer file.Close()
			if openFileErr != nil {
				logrus.Error("error: MetricBlkIO: open file failed: ", metricFile, openFileErr)
			}
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := scanner.Text()
				lineSlice := strings.Fields(line)
				if len(lineSlice) == 3 {
					metricSuffix, value := strings.ToLower(lineSlice[1]), lineSlice[2]
					MetricToChannel(metric, metricPrefix+"."+metricSuffix, value)
				}
			}
		}(metricItem, metricPrefix, metricFile)
	}
}

func MetricToChannel(metric Metric, name string, value interface{}) {
	metric.Name = Prefix + "." + name
	metric.Value = value
	MetricChannel <- metric
}

func MetricSender() {
	for metric := range MetricChannel {
		logrus.Infof("%s %s %s %d", metric.Timestamp, metric.CounterType, metric.Name, metric.Value)
	}
}

func fileToMap(filePath string) (map[string]int64, error) {
	result := map[string]int64{}
	file, openFileErr := os.Open(filePath)
	defer file.Close()
	if openFileErr != nil {
		return result, errors.New("open file failed")
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		s := strings.Fields(scanner.Text())
		if len(s) == 2 {
			value, convertErr := strconv.ParseInt(s[1], 10, 64)
			if convertErr != nil {
				logrus.Errorf("converting data failed %v: ", s)
				continue
			}
			result[s[0]] = value
		}
	}
	return result, nil
}

func getLine(filePath string) (interface{}, error) {
	file, openFileErr := os.Open(filePath)
	defer file.Close()
	if openFileErr != nil {
		return nil, errors.New("func getLine(): open file failed")
	}

	reader := bufio.NewReader(file)
	resultByte, _, _ := reader.ReadLine()
	resultString := string(resultByte)
	resultInt, convertErr := strconv.ParseInt(resultString, 10, 64)
	if convertErr != nil {
		return resultString, nil
	}
	return resultInt, nil
}

func getMyID() (string, error) {
	data, getLineErr := getLine("/proc/self/cpuset")
	if getLineErr != nil {
		return "", getLineErr
	}
	dataSlice := strings.Split(data.(string), "/")
	if len(dataSlice) == 3 {
		return dataSlice[2], nil
	} else {
		return "", errors.New("func getMyID(): data format not match")
	}
}

func setLogToFile(logFileHandler *os.File) {
	logrus.SetOutput(logFileHandler)
	logrus.SetFormatter(&logrus.TextFormatter{})
	logrus.SetLevel(logrus.DebugLevel)
}

func main() {
	logFileHandler, openLogErr := os.OpenFile(LogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0755)
	defer logFileHandler.Close()
	if openLogErr != nil {
		logrus.Fatal("open log file failed: ", openLogErr)
		return
	}
	setLogToFile(logFileHandler)

	myID, getIDErr := getMyID()
	if getIDErr != nil {
		logrus.Fatal(getIDErr)
		return
	}

	go MetricSender()

	t := time.NewTicker(Step * time.Second)
	for {
		metric := mewMetric()
		go MetricCpu(myID, metric)
		go MetricMemory(myID, metric)
		go MetricBlkIO(myID, metric)
		go MetricConnection(metric)
		go MetricNetPacket(metric)
		go MetricProgress(metric)
		go MetricPort(metric)
		go MetricAgentAlive(metric)
		<-t.C
	}
}
