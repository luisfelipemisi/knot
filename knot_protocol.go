package knot

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/luisfelipemisi/knot/config"
	"github.com/luisfelipemisi/knot/integration/knot/entities"
	"github.com/luisfelipemisi/knot/integration/knot/network"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

// Protocol interface provides methods to handle KNoT Protocol
type Protocol interface {
	Close() error
	createDevice(device entities.Device) error
	deleteDevice(id string) error
	updateDevice(device entities.Device) error
	checkData(device entities.Device) error
	checkDeviceConfiguration(device entities.Device) error
	deviceExists(device entities.Device) bool
	generateID(device entities.Device) (string, error)
	checkTimeout(device entities.Device, log *logrus.Entry) entities.Device
	requestsKnot(deviceChan chan entities.Device, device entities.Device, oldState string, curState string, message string, log *logrus.Entry)
}
type networkWrapper struct {
	amqp       *network.AMQP
	publisher  network.Publisher
	subscriber network.Subscriber
}

type protocol struct {
	userToken string
	network   *networkWrapper
	devices   map[string]entities.Device
}

func newProtocol(pipeDevices chan map[string]entities.Device, conf config.IntegrationKNoTConfig, deviceChan chan entities.Device, msgChan chan network.InMsg, log *logrus.Entry, devices map[string]entities.Device) (Protocol, error) {
	p := &protocol{}

	p.userToken = conf.UserToken
	p.network = new(networkWrapper)
	p.network.amqp = network.NewAMQP(conf.URL)
	err := p.network.amqp.Start()
	if err != nil {
		log.Println("Knot connection error")
		return p, err
	} else {
		log.Println("Knot connected")
	}
	p.network.publisher = network.NewMsgPublisher(p.network.amqp)
	p.network.subscriber = network.NewMsgSubscriber(p.network.amqp)

	if err = p.network.subscriber.SubscribeToKNoTMessages(msgChan); err != nil {
		log.Errorln("Error to subscribe")
		return p, err
	}
	p.devices = make(map[string]entities.Device)
	p.devices = devices

	go handlerKnotAMQP(msgChan, deviceChan, log)
	go dataControl(pipeDevices, deviceChan, p, log)

	return p, nil
}

// Check for data to be updated
func (p *protocol) checkData(device entities.Device) error {
	var ok bool
	id_pass := 0
	ok = false
	// Check if the ids are correct, no repetition
	for _, data := range device.Data {
		if data.SensorID != id_pass {
			id_pass = data.SensorID
			ok = true
		} else {
			ok = false
		}
		if data.TimeStamp == nil {
			ok = false
		}
		if data.Value == nil {
			ok = false
		}
		if !ok {
			return fmt.Errorf("Invalid Data")
		}
	}
	if ok {
		return nil
	}
	return fmt.Errorf("Invalid Data")
}

// Check for device configuration
func (p *protocol) checkDeviceConfiguration(device entities.Device) error {
	var ok bool
	id_pass := 0
	// Check if the ids are correct, no repetition
	for _, data := range device.Config {
		if data.SensorID != id_pass {
			id_pass = data.SensorID
			ok = true
		} else {
			ok = false
		}
	}
	if ok {
		return nil
	}
	return fmt.Errorf("Invalid Config")
}

// Update the knot device information on map
func (p *protocol) updateDevice(device entities.Device) error {
	if _, checkDevice := p.devices[device.ID]; !checkDevice {

		return fmt.Errorf("Device do not exist")
	}

	receiver := p.devices[device.ID]

	if p.checkDeviceConfiguration(device) == nil {
		receiver.Config = device.Config
	}
	if device.Name != "" {
		receiver.Name = device.Name
	}
	if device.Token != "" {
		receiver.Token = device.Token
	}
	if device.Error != "" {
		receiver.Error = device.Error
	}

	receiver.Data = nil
	oldState := receiver.State
	receiver.State = entities.KnotNew
	p.devices[device.ID] = receiver

	data, err := yaml.Marshal(&p.devices)
	if err != nil {
		log.Fatal(err)
	}

	err = os.WriteFile("internal/config/device_config.yaml", data, 0600)
	if err != nil {
		log.Fatal(err)
	}
	receiver.State = oldState
	if device.State != "" {
		receiver.State = device.State
	}
	if p.checkData(device) == nil {
		receiver.Data = device.Data
	}
	p.devices[device.ID] = receiver

	return nil
}

// Close closes the protocol.
func (p *protocol) Close() error {
	p.network.amqp.Stop()
	return nil
}

// Create a new knot device
func (p *protocol) createDevice(device entities.Device) error {

	if device.State != "" {
		return fmt.Errorf("device cannot be created, unknown source")
	} else {

		device.State = entities.KnotNew

		p.devices[device.ID] = device

		return nil
	}
}

// Create a new device ID
func (p *protocol) generateID(device entities.Device) (string, error) {
	delete(p.devices, device.ID)
	var err error
	device.ID, err = tokenIDGenerator()
	device.Token = ""
	p.devices[device.ID] = device

	log.Print(" generated a new Device ID : ")
	log.Println(device.ID)

	return device.ID, err
}

// Check if the device exists
func (p *protocol) deviceExists(device entities.Device) bool {

	if _, checkDevice := p.devices[device.ID]; checkDevice {

		return true
	}
	return false
}

// Generated a new Device ID
func tokenIDGenerator() (string, error) {
	b := make([]byte, 8)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// Just formart the Error message
func errorFormat(device entities.Device, strError string) entities.Device {
	device.Error = strError
	device.State = entities.KnotError
	return device
}

// Delete the knot device from map
func (p *protocol) deleteDevice(id string) error {
	if _, d := p.devices[id]; !d {
		return fmt.Errorf("Device do not exist")
	}

	delete(p.devices, id)
	return nil
}

//non-blocking channel to update devices on the other routin
func updateDeviceMap(pipeDevices chan map[string]entities.Device, devices map[string]entities.Device) {
	pipeDevices <- devices
}

func verifyErrors(err error, log *logrus.Entry) {
	if err != nil {
		log.Errorln(err)
	}
}

//init the timeout couter
func initTimeout(deviceChan chan entities.Device, device entities.Device) {
	go func(deviceChan chan entities.Device, device entities.Device) {
		time.Sleep(20 * time.Second)
		device.Error = "timeOut"
		deviceChan <- device
	}(deviceChan, device)
}

// check response time
func (p *protocol) requestsKnot(deviceChan chan entities.Device, device entities.Device, oldState string, curState string, message string, log *logrus.Entry) {
	device.State = oldState
	initTimeout(deviceChan, device)
	device.State = curState
	err := p.updateDevice(device)
	if err != nil {
		log.Errorln(err)
	} else {
		log.Println(message)
		switch oldState {
		case entities.KnotNew:
			err = p.network.publisher.PublishDeviceRegister(p.userToken, &device)
		case entities.KnotRegistered:
			err = p.network.publisher.PublishDeviceAuth(p.userToken, &device)
		case entities.KnotAuth:
			err = p.network.publisher.PublishDeviceUpdateConfig(p.userToken, &device)
		}
		verifyErrors(err, log)
	}
}

// Control device paths
func dataControl(pipeDevices chan map[string]entities.Device, deviceChan chan entities.Device, p *protocol, log *logrus.Entry) {
	pipeDevices <- p.devices

	for device := range deviceChan {

		if !p.deviceExists(device) {
			if device.Error != "timeOut" {
				log.Error("device id received does not match the stored")
			}
		} else {

			device = p.checkTimeout(device, log)
			if device.State != entities.KnotOff && device.Error != "timeOut" {

				err := p.updateDevice(device)
				verifyErrors(err, log)
				device = p.devices[device.ID]

				if device.Name == "" {
					log.Fatalln("Device has no name")
				} else if device.State == entities.KnotNew {
					if device.Token != "" {
						device.State = entities.KnotRegistered
					} else {
						id, err := p.generateID(device)
						if err != nil {
							device.State = entities.KnotOff
							log.Error(err)
						} else {
							device.State = entities.KnotNew
							device.ID = id
							err = p.updateDevice(device)
							if err != nil {
								log.Error(err)
							}
							go updateDeviceMap(pipeDevices, p.devices)
						}
					}
				}
			} else if device.Error == "timeOut" {
				device.Error = ""
			}
			switch device.State {

			// If the device status is new, request a device registration
			case entities.KnotNew:

				p.requestsKnot(deviceChan, device, device.State, entities.KnotWaitReg, "send a register request", log)

			// If the device is already registered, ask for device authentication
			case entities.KnotRegistered:

				p.requestsKnot(deviceChan, device, device.State, entities.KnotWaitAuth, "send a auth request", log)

			// The device has a token and authentication was successful.
			case entities.KnotAuth:

				p.requestsKnot(deviceChan, device, device.State, entities.KnotWaitConfig, "send a updateconfig request", log)

			//everything is ok with knot device
			case entities.KnotReady:
				device.State = entities.KnotPublishing
				err := p.updateDevice(device)
				if err != nil {
					log.Errorln(err)
				} else {
					go updateDeviceMap(pipeDevices, p.devices)
				}
			// Send the new data that comes from the device to Knot Cloud
			case entities.KnotPublishing:
				if p.checkData(device) == nil {
					log.Println("send data of device ", device.Data[0].SensorID)

					err := p.network.publisher.PublishDeviceData(p.userToken, &device, device.Data)
					if err != nil {
						log.Errorln(err)
					} else {
						device.Data = nil
						err = p.updateDevice(device)
						verifyErrors(err, log)
					}
				} else {
					log.Println("invalid data, has no data to send")
				}

			// If the device is already registered, ask for device authentication
			case entities.KnotAlreadyReg:

				var err error
				if device.Token == "" {
					device.ID, err = p.generateID(device)
					if err != nil {
						log.Error(err)
					} else {
						go updateDeviceMap(pipeDevices, p.devices)
						p.requestsKnot(deviceChan, device, entities.KnotNew, entities.KnotWaitReg, "send a register request", log)
					}
				} else {

					p.requestsKnot(deviceChan, device, entities.KnotRegistered, entities.KnotWaitAuth, "send a Auth request", log)

				}

			// Just delete
			case entities.KnotForceDelete:
				var err error
				log.Println("delete a device")

				device.ID, err = p.generateID(device)
				if err != nil {
					log.Error(err)
				} else {
					go updateDeviceMap(pipeDevices, p.devices)
					p.requestsKnot(deviceChan, device, entities.KnotNew, entities.KnotWaitReg, "send a register request", log)
				}

			// Handle errors
			case entities.KnotError:
				log.Println("ERROR: ")
				switch device.Error {
				// If the device is new to the chirpstack platform, but already has a registration in Knot, first the device needs to ask to unregister and then ask for a registration.
				case "thing's config not provided":
					log.Println("thing's config not provided")

				default:
					log.Println("ERROR WITHOUT HANDLER" + device.Error)

				}
				device.State = entities.KnotNew
				device.Error = ""
				err := p.updateDevice(device)
				verifyErrors(err, log)

			// ignore the device
			case entities.KnotOff:

			}

		}
	}
}

//check if response was received by comparing previous state with the new one
func (p *protocol) checkTimeout(device entities.Device, log *logrus.Entry) entities.Device {

	if device.Error == "timeOut" {
		curDevice := p.devices[device.ID]
		if device.State == entities.KnotNew && curDevice.State == entities.KnotWaitReg {
			log.Println("error: TimeOut")
			return device
		} else if device.State == entities.KnotRegistered && curDevice.State == entities.KnotWaitAuth {
			log.Println("error: TimeOut")
			return device
		} else if device.State == entities.KnotAuth && curDevice.State == entities.KnotWaitConfig {
			log.Println("error: TimeOut")
			return device
		} else {
			device.State = entities.KnotOff
			return device
		}
	}
	return device
}

// Handle amqp messages
func handlerAMQPmessage(message network.InMsg, log *logrus.Entry) entities.Device {
	receiver := network.DeviceGenericMessage{}
	device := entities.Device{}
	err := json.Unmarshal([]byte(string(message.Body)), &receiver)
	verifyErrors(err, log)
	device.ID = receiver.ID
	device.Name = receiver.Name
	device.Error = receiver.Error
	if network.BindingKeyRegistered == message.RoutingKey && receiver.Token != "" {
		device.Token = receiver.Token
	}
	return device
}

// Handles messages coming from AMQP
func handlerKnotAMQP(msgChan <-chan network.InMsg, deviceChan chan entities.Device, log *logrus.Entry) {

	for message := range msgChan {

		switch message.RoutingKey {

		// Registered msg from knot
		case network.BindingKeyRegistered:

			device := handlerAMQPmessage(message, log)

			if device.Error != "" {
				// Alread registered
				log.Println("received a registration response with a error")
				if device.Error == "thing is already registered" {
					device.State = entities.KnotAlreadyReg
					deviceChan <- device
				} else {
					deviceChan <- errorFormat(device, device.Error)
				}
			} else {
				log.Println("received a registration response with no error")
				device.State = entities.KnotRegistered
				deviceChan <- device
			}

		// Unregistered
		case network.BindingKeyUnregistered:
			log.Println("received a unregistration response")
			device := handlerAMQPmessage(message, log)
			device.State = entities.KnotForceDelete

			deviceChan <- device

		// Receive a auth msg
		case network.ReplyToAuthMessages:
			device := handlerAMQPmessage(message, log)

			if device.Error != "" {
				// Alread registered
				log.Println("received a authentication response with a error")
				device.State = entities.KnotForceDelete
				deviceChan <- device
			} else {
				log.Println("received a authentication response with no error")
				device.State = entities.KnotAuth
				deviceChan <- device

			}
		case network.BindingKeyUpdatedConfig:

			device := handlerAMQPmessage(message, log)

			if device.Error == "failed to validate if config is valid: error getting thing metadata: thing not found on thing's service" {

				log.Println("sent the configuration again")
				device.State = entities.KnotAuth
				deviceChan <- device

			} else if device.Error != "" {
				log.Println("received a config update response with a error")
				deviceChan <- errorFormat(device, device.Error)
			} else {
				log.Println("received a config update response with no error")
				device.State = entities.KnotReady
				deviceChan <- device
			}
		}
	}
}
