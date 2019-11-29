package director

import (
	"bytes"
	"encoding/json"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"

	"github.com/mdmdirector/mdmdirector/db"
	"github.com/mdmdirector/mdmdirector/types"
	"github.com/mdmdirector/mdmdirector/utils"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
)

const (
	MAX              = 5
	DelaySeconds     = 7200
	HalfDelaySeconds = 7200 / 2
)

var DevicesFetchedFromMDM bool

func RetryCommands() error {
	var delay time.Duration = 120
	if utils.DebugMode() {
		delay = 20
	}
	ticker := time.NewTicker(delay * time.Second)
	defer ticker.Stop()
	fn := func() error {
		err := pushNotNow()
		if err != nil {
			return err
		}
		return nil
	}

	err := fn()
	if err != nil {
		return err
	}

	for range ticker.C {
		err := fn()
		if err != nil {
			return err
		}
	}
	return nil
}

func pushNotNow() error {
	var command types.Command
	var commands []types.Command
	err := db.DB.Model(&command).Select("DISTINCT(device_ud_id)").Where("status = ?", "NotNow").Scan(&commands).Error
	if err != nil {
		return err
	}

	client := &http.Client{}

	for _, queuedCommand := range commands {

		endpoint, err := url.Parse(utils.ServerURL())
		if err != nil {
			log.Error(err)
		}
		retry := time.Now().Unix() + 3600
		endpoint.Path = path.Join(endpoint.Path, "push", queuedCommand.DeviceUDID)
		queryString := endpoint.Query()
		queryString.Set("expiration", strconv.FormatInt(retry, 10))
		endpoint.RawQuery = queryString.Encode()
		req, err := http.NewRequest("GET", endpoint.String(), nil)
		if err != nil {
			log.Error(err)
		}
		req.SetBasicAuth("micromdm", utils.APIKey())

		resp, err := client.Do(req)
		if err != nil {
			log.Error(err)
			continue
		}

		resp.Body.Close()
	}
	return nil
}

func shuffleDevices(vals []types.Device) []types.Device {
	r := rand.New(rand.NewSource(time.Now().Unix()))
	ret := make([]types.Device, len(vals))
	perm := r.Perm(len(vals))
	for i, randIndex := range perm {
		ret[i] = vals[randIndex]
	}
	return ret
}

func pushAll() error {
	var devices []types.Device
	var dbDevices []types.Device
	now := time.Now()

	threeHoursAgo := time.Now().Add(-3 * time.Hour)
	lastCheckinDelay := time.Now().Add(-HalfDelaySeconds * time.Second)

	err := db.DB.Find(&dbDevices).Scan(&dbDevices).Error
	if err != nil {
		return err
	}

	for _, dbDevice := range dbDevices {

		// If it's been updated within the last three hours, try to push again as it might still be online
		if dbDevice.LastCheckedIn.After(threeHoursAgo) {
			log.Infof("%v checked in more than three hours ago", dbDevice.UDID)
			if now.Before(dbDevice.NextPush) {
				log.Infof("Not pushing to %v, next push is %v", dbDevice.UDID, dbDevice.NextPush)
				continue
			}
		}
		// This contrived bit of logic is to handle devices that don't have a LastScheduledPush set yet
		if !dbDevice.LastScheduledPush.Before(lastCheckinDelay) {
			log.Infof("%v last pushed in %v which is within %v seconds", dbDevice.UDID, dbDevice.LastScheduledPush, HalfDelaySeconds)
			continue
		}

		devices = append(devices, dbDevice)
	}

	client := &http.Client{}

	log.Debug("Pushing to all in debug mode")
	sem := make(chan int, MAX)
	counter := 0
	total := 0
	devicesPerSecond := float64(len(devices)) / float64((DelaySeconds - 1))
	shuffledDevices := shuffleDevices(devices)
	for i := range shuffledDevices {
		device := shuffledDevices[i]
		if float64(counter) >= devicesPerSecond {
			log.Infof("Sleeping due to having processed %v devices out of %v. Processing %v per 0.5 seconds.", total, len(devices), devicesPerSecond)
			time.Sleep(500 * time.Millisecond)
			counter = 0
		}
		log.Debug("Processed ", counter)
		sem <- 1 // will block if there is MAX ints in sem
		go func() {
			pushConcurrent(device, client)
			<-sem // removes an int from sem, allowing another to proceed
		}()
		counter++
		total++
	}
	log.Infof("Completed pushing to %v devices", len(devices))
	return nil
}

func pushConcurrent(device types.Device, client *http.Client) {
	now := time.Now()
	var retry int64 = time.Now().Unix() + DelaySeconds
	endpoint, err := url.Parse(utils.ServerURL())
	if err != nil {
		log.Error(err)
	}

	log.Infof("Pushing to %v", device.UDID)

	if now.After(device.NextPush) {
		log.Infof("After scheduled push of %v for %v. Pushing with an expiry of 24 hours", device.NextPush, device.UDID)
		retry = time.Now().Unix() + 86400
	}

	endpoint.Path = path.Join(endpoint.Path, "push", device.UDID)
	queryString := endpoint.Query()
	queryString.Set("expiration", strconv.FormatInt(retry, 10))
	endpoint.RawQuery = queryString.Encode()
	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		log.Error(err)
	}
	req.SetBasicAuth("micromdm", utils.APIKey())

	resp, err := client.Do(req)
	if err != nil {
		log.Error(err)
	}

	err = db.DB.Model(&device).Where("ud_id = ?", device.UDID).Updates(types.Device{
		LastScheduledPush: now,
		NextPush:          time.Now().Add(12 * time.Hour),
	}).Error
	if err != nil {
		log.Error(err)
	}

	resp.Body.Close()
}

func PushDevice(udid string) error {
	client := &http.Client{}

	endpoint, err := url.Parse(utils.ServerURL())
	if err != nil {
		return errors.Wrap(err, "PushDevice")
	}

	retry := time.Now().Unix() + 3600

	endpoint.Path = path.Join(endpoint.Path, "push", udid)
	queryString := endpoint.Query()
	queryString.Set("expiration", strconv.FormatInt(retry, 10))
	endpoint.RawQuery = queryString.Encode()
	req, err := http.NewRequest("GET", endpoint.String(), nil)
	if err != nil {
		return errors.Wrap(err, "PushDevice")
	}
	req.SetBasicAuth("micromdm", utils.APIKey())

	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "PushDevice")
	}

	err = resp.Body.Close()
	if err != nil {
		return errors.Wrap(err, "PushDevice")
	}

	return nil
}

func UnconfiguredDevices() {
	ticker := time.NewTicker(30 * time.Second)

	defer ticker.Stop()
	fn := func() {
		err := processUnconfiguredDevices()
		if err != nil {
			log.Error(err)
		}
	}

	fn()
	for range ticker.C {
		fn()
	}
	// for {
	// 	select {
	// 	case <-ticker.C:
	// 		fn()
	// 	}
	// }
}

func processUnconfiguredDevices() error {
	var awaitingConfigDevices []types.Device
	var awaitingConfigDevice types.Device

	// thirtySecondsAgo := time.Now().Add(-30 * time.Second)

	err := db.DB.Model(&awaitingConfigDevice).Where("awaiting_configuration = ?", true).Scan(&awaitingConfigDevices).Error
	if err != nil {
		return err
	}

	// if len(awaitingConfigDevices) == 0 {
	// 	log.Debug("No unconfigured devices")
	// 	return nil
	// }

	for i := range awaitingConfigDevices {
		unconfiguredDevice := awaitingConfigDevices[i]
		log.Debugf("Running initial tasks due to schedule %v", unconfiguredDevice.UDID)
		err := RunInitialTasks(unconfiguredDevice.UDID)
		if err != nil {
			log.Error(err)
		}
	}

	return nil
}

func ScheduledCheckin() {
	// var delay time.Duration
	ticker := time.NewTicker(DelaySeconds * time.Second)
	if utils.DebugMode() {
		ticker = time.NewTicker(20 * time.Second)
	}

	for {
		if !DevicesFetchedFromMDM {
			time.Sleep(30 * time.Second)
			log.Info("Devices are still being fetched from MicroMDM")
		} else {
			break
		}
	}

	defer ticker.Stop()
	fn := func() {
		log.Infof("Running scheduled checkin (%v second) delay", DelaySeconds)
		err := processScheduledCheckin()
		if err != nil {
			log.Error(err)
		}
	}

	fn()

	for range ticker.C {
		go fn()
	}
}

func processScheduledCheckin() error {
	if utils.DebugMode() {
		log.Debug("Processing scheduledCheckin in debug mode")
	}

	err := pushAll()
	if err != nil {
		return err
	}

	var certificates []types.Certificate

	err = db.DB.Unscoped().Model(&certificates).Where("device_ud_id is NULL").Delete(&types.Certificate{}).Error
	if err != nil {
		return errors.Wrap(err, "processScheduledCheckin::CleanupNullCertificates")
	}

	return nil
}

func FetchDevicesFromMDM() {
	var deviceModel types.Device
	var devices types.DevicesFromMDM
	log.Info("Fetching devices from MicroMDM...")

	// Handle Micro having a bad day
	client := &http.Client{
		Timeout: time.Second * 60,
	}

	endpoint, err := url.Parse(utils.ServerURL())
	if err != nil {
		log.Error(err)
	}
	endpoint.Path = path.Join(endpoint.Path, "v1", "devices")

	req, _ := http.NewRequest("POST", endpoint.String(), bytes.NewBufferString("{}"))
	req.SetBasicAuth("micromdm", utils.APIKey())
	resp, err := client.Do(req)
	if err != nil {
		log.Error(err)
	}

	if resp.StatusCode != 200 {
		return
	}

	defer resp.Body.Close()

	responseData, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		log.Error(err)
	}

	err = json.Unmarshal(responseData, &devices)
	if err != nil {
		log.Error(err)
	}

	for _, newDevice := range devices.Devices {
		var device types.Device
		device.UDID = newDevice.UDID
		device.SerialNumber = newDevice.SerialNumber
		device.Active = newDevice.EnrollmentStatus
		if newDevice.EnrollmentStatus {
			device.AuthenticateRecieved = true
			device.TokenUpdateRecieved = true
			device.InitialTasksRun = true
		}
		if newDevice.UDID == "" {
			continue
		}
		err := db.DB.Model(&deviceModel).Where("ud_id = ?", newDevice.UDID).FirstOrCreate(&device).Error
		if err != nil {
			log.Error(err)
		}

	}
	DevicesFetchedFromMDM = true
	log.Info("Finished fetching devices from MicroMDM...")
}
