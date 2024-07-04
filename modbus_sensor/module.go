package modbus_sensor

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/simonvetter/modbus"
	"go.viam.com/rdk/components/sensor"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"

	"viam-modbus/common"
	"viam-modbus/utils"
)

var Model = resource.NewModel("viam-soleng", "sensor", "modbus-tcp")

// Registers the sensor model
func init() {
	resource.RegisterComponent(
		sensor.API,
		Model,
		resource.Registration[sensor.Sensor, *ModbusSensorConfig]{
			Constructor: NewModbusSensor,
		},
	)
}

// Creates a new modbus sensor instance
func NewModbusSensor(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (sensor.Sensor, error) {
	logger.Infof("Starting Modbus Sensor Component %v", utils.Version)
	c, cancelFunc := context.WithCancel(context.Background())
	b := ModbusSensor{
		Named:      conf.ResourceName().AsNamed(),
		logger:     logger,
		cancelFunc: cancelFunc,
		ctx:        c,
	}

	if err := b.Reconfigure(ctx, deps, conf); err != nil {
		return nil, err
	}
	return &b, nil
}

type ModbusSensor struct {
	resource.Named
	mu               sync.RWMutex
	logger           logging.Logger
	cancelFunc       context.CancelFunc
	ctx              context.Context
	client           *common.ViamModbusClient
	blocks           []ModbusBlocks
	holdingRegisters []ModbusBlocks
	welding_job      welding_job
}

// Holds welding job information
type welding_job struct {
	welding     bool
	job_id      string
	createJobID bool
}

// Returns modbus register values
func (r *ModbusSensor) Readings(ctx context.Context, extra map[string]interface{}) (map[string]interface{}, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.client == nil {
		return nil, errors.New("modbus client not initialized")
	}
	results := map[string]interface{}{}

	// Extract address range from holding registers
	offset := r.holdingRegisters[0].Offset
	length := (r.holdingRegisters[len(r.holdingRegisters)-1].Offset + r.holdingRegisters[len(r.holdingRegisters)-1].Length) - offset
	r.logger.Debugf("Reading holding registers: Offset %v Length %v", offset, length)

	b, err := r.client.ReadHoldingRegisters(uint16(offset), uint16(length))
	if err != nil {
		return nil, err
	}

	// Map holding register names to values
	for i, block := range r.holdingRegisters {
		//value := b[block.Offset:block.Length]
		results[block.Name] = strconv.Itoa(int(b[i]))
	}

	// Check if set_job_id is enabled
	if r.welding_job.createJobID {
		// Check if welding job is active and add a job_id if it is
		if results["Status"] == "51" && !r.welding_job.welding {
			// start of welding process
			//r.logger.Infof("if1 job status: %v", r.welding_job.welding)
			r.welding_job.welding = true
			r.welding_job.job_id = uuid.New().String()
			results["job_id"] = r.welding_job.job_id
		} else if results["Status"] == "51" && r.welding_job.welding {
			// Still welding
			results["job_id"] = r.welding_job.job_id
		} else {
			// Stop welding or not welding anyway
			//r.logger.Infof("else job status: %v", r.welding_job.welding)
			r.welding_job.welding = false
			r.welding_job.job_id = ""
			results["job_id"] = ""
		}
	}

	return results, nil
}

// Closes the modbus sensor instance
func (r *ModbusSensor) Close(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logger.Info("Closing Modbus Sensor Component")
	r.cancelFunc()
	if r.client != nil {
		err := r.client.Close()
		if err != nil {
			r.logger.Errorf("Failed to close modbus client: %#v", err)
		}
	}
	return nil
}

// DoCommand currently not implemented
func (*ModbusSensor) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return map[string]interface{}{"ok": 1}, nil
}

// Configures the modbus sensor instance
func (r *ModbusSensor) Reconfigure(ctx context.Context, deps resource.Dependencies, conf resource.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logger.Debug("Reconfiguring Modbus Sensor Component")

	newConf, err := resource.NativeConfig[*ModbusSensorConfig](conf)
	if err != nil {
		return err
	}

	// In case the module has changed name
	r.Named = conf.ResourceName().AsNamed()

	// Extract holding registers from configured blocks
	for _, block := range newConf.Blocks {
		if block.Type == "holding_registers" {
			r.holdingRegisters = append(r.holdingRegisters, block)
		}
	}
	// Sort holding registers by offset
	sort.Slice(r.holdingRegisters, func(i, j int) bool {
		return r.holdingRegisters[i].Offset < r.holdingRegisters[j].Offset
	})
	r.logger.Infof("Holding Registers: %v", r.holdingRegisters)

	return r.reconfigure(newConf, deps)
}

func (r *ModbusSensor) reconfigure(newConf *ModbusSensorConfig, _ resource.Dependencies) error {
	r.logger.Infof("Reconfiguring Modbus Sensor Component with %v", newConf)
	if r.client != nil {
		err := r.client.Close()
		if err != nil {
			r.logger.Errorf("Failed to close modbus client: %#v", err)
			// TODO: should we exit here?
		}
	}
	// Reset welding job
	r.welding_job = welding_job{}

	// Set create job_id flag
	r.welding_job.createJobID = newConf.CreateJobID

	endianness, err := common.GetEndianness(newConf.Modbus.Endianness)
	if err != nil {
		return err
	}

	wordOrder, err := common.GetWordOrder(newConf.Modbus.WordOrder)
	if err != nil {
		return err
	}

	timeout := time.Millisecond * time.Duration(newConf.Modbus.Timeout)

	clientConfig := modbus.ClientConfiguration{
		URL:      newConf.Modbus.URL,
		Speed:    newConf.Modbus.Speed,
		DataBits: newConf.Modbus.DataBits,
		Parity:   newConf.Modbus.Parity,
		StopBits: newConf.Modbus.StopBits,
		Timeout:  timeout,
		// TODO: To be implemented
		//TLSClientCert: tlsClientCert,
		//TLSRootCAs:    tlsRootCAs,
	}
	client, err := common.NewModbusClient(r.logger, endianness, wordOrder, clientConfig)
	if err != nil {
		return err
	}
	r.client = client

	r.blocks = newConf.Blocks
	return nil
}
