/*
	Copyright (c) 2015-2016 Christopher Young
	Distributable under the terms of The "BSD New" License
	that can be found in the LICENSE file, herein included
	as part of this header.

	gps.go: GPS functions, GPS init, AHRS status messages, other external sensor monitoring.
*/

package main

import (
	"fmt"
	"log"
	"math"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"bufio"

	"github.com/tarm/serial"

	"os"
	"os/exec"
)

const (
	SAT_TYPE_UNKNOWN = 0  // default type
	SAT_TYPE_GPS     = 1  // GPxxx; NMEA IDs 1-32
	SAT_TYPE_GLONASS = 2  // GLxxx; NMEA IDs 65-96
	SAT_TYPE_GALILEO = 3  // GAxxx; NMEA IDs
	SAT_TYPE_BEIDOU  = 4  // GBxxx; NMEA IDs 201-235
	SAT_TYPE_QZSS    = 5  // QZSS
	SAT_TYPE_SBAS    = 10 // NMEA IDs 33-54
)

const (
	BARO_TYPE_NONE         = 0 // No baro present
	BARO_TYPE_BMP280       = 1 // Stratux AHRS module or similar internal baro
	BARO_TYPE_OGNTRACKER   = 2 // OGN Tracker with baro pressure
	BARO_TYPE_NMEA         = 3 // Other NMEA provider that reports $PGRMZ (SoftRF)
	BARO_TYPE_ADSBESTIMATE = 4 // If we have no baro, we will try to estimate baro pressure from ADS-B targets reporting GnssDiffFromBaroAlt (HAE<->Baro difference)
)

type SatelliteInfo struct {
	SatelliteNMEA    uint8     // NMEA ID of the satellite. 1-32 is GPS, 33-54 is SBAS, 65-88 is Glonass.
	SatelliteID      string    // Formatted code indicating source and PRN code. e.g. S138==WAAS satellite 138, G2==GPS satellites 2
	Elevation        int16     // Angle above local horizon, -xx to +90
	Azimuth          int16     // Bearing (degrees true), 0-359
	Signal           int8      // Signal strength, 0 - 99; -99 indicates no reception
	Type             uint8     // Type of satellite (GPS, GLONASS, Galileo, SBAS)
	TimeLastSolution time.Time // Time (system ticker) a solution was last calculated using this satellite
	TimeLastSeen     time.Time // Time (system ticker) a signal was last received from this satellite
	TimeLastTracked  time.Time // Time (system ticker) this satellite was tracked (almanac data)
	InSolution       bool      // True if satellite is used in the position solution (reported by GSA message or PUBX,03)
}

type SituationData struct {
	// From GPS.
	muGPS                       *sync.Mutex
	muGPSPerformance            *sync.Mutex
	muSatellite                 *sync.Mutex
	GPSLastFixSinceMidnightUTC  float32
	GPSLatitude                 float32
	GPSLongitude                float32
	GPSFixQuality               uint8
	GPSHeightAboveEllipsoid     float32 // GPS height above WGS84 ellipsoid, ft. This is specified by the GDL90 protocol, but most EFBs use MSL altitude instead. HAE is about 70-100 ft below GPS MSL altitude over most of the US.
	GPSGeoidSep                 float32 // geoid separation, ft, MSL minus HAE (used in altitude calculation)
	GPSSatellites               uint16  // satellites used in solution
	GPSSatellitesTracked        uint16  // satellites tracked (almanac data received)
	GPSSatellitesSeen           uint16  // satellites seen (signal received)
	GPSHorizontalAccuracy       float32 // 95% confidence for horizontal position, meters.
	GPSNACp                     uint8   // NACp categories are defined in AC 20-165A
	GPSAltitudeMSL              float32 // Feet MSL
	GPSVerticalAccuracy         float32 // 95% confidence for vertical position, meters
	GPSVerticalSpeed            float32 // GPS vertical velocity, feet per second
	GPSLastFixLocalTime         time.Time
	GPSTrueCourse               float32
	GPSTurnRate                 float64 // calculated GPS rate of turn, degrees per second
	GPSGroundSpeed              float64
	GPSLastGroundTrackTime      time.Time
	GPSTime                     time.Time
	GPSLastGPSTimeStratuxTime   time.Time // stratuxClock time since last GPS time received.
	GPSLastValidNMEAMessageTime time.Time // time valid NMEA message last seen
	GPSLastValidNMEAMessage     string    // last NMEA message processed.
	GPSPositionSampleRate       float64   // calculated sample rate of GPS positions

	// From pressure sensor.
	muBaro                  *sync.Mutex
	BaroTemperature         float32
	BaroPressureAltitude    float32
	BaroVerticalSpeed       float32
	BaroLastMeasurementTime time.Time
	BaroSourceType          uint8

	// From AHRS source.
	muAttitude           *sync.Mutex
	AHRSPitch            float64
	AHRSRoll             float64
	AHRSGyroHeading      float64
	AHRSMagHeading       float64
	AHRSSlipSkid         float64
	AHRSTurnRate         float64
	AHRSGLoad            float64
	AHRSGLoadMin         float64
	AHRSGLoadMax         float64
	AHRSLastAttitudeTime time.Time
	AHRSStatus           uint8
}

/*
myGPSPerfStats used to track short-term position / velocity trends, used to feed dynamic AHRS model. Use floats for better resolution of calculated data.
*/
type gpsPerfStats struct {
	stratuxTime   uint64  // time since Stratux start, msec
	nmeaTime      float32 // timestamp from NMEA message
	msgType       string  // NMEA message type
	gsf           float32 // knots
	coursef       float32 // true course [degrees]
	alt           float32 // gps altitude, ft msl
	vv            float32 // vertical velocity, ft/sec
	gpsTurnRate   float64 // calculated turn rate, deg/sec. Right turn is positive.
	gpsPitch      float64 // estimated pitch angle, deg. Calculated from gps ground speed and VV. Equal to flight path angle.
	gpsRoll       float64 // estimated roll angle from turn rate and groundspeed, deg. Assumes airplane in coordinated turns.
	gpsLoadFactor float64 // estimated load factor from turn rate and groundspeed, "gee". Assumes airplane in coordinated turns.
	//TODO: valid/invalid flag.
}

var gpsPerf gpsPerfStats
var myGPSPerfStats []gpsPerfStats
var gpsTimeOffsetPpsMs = 100.0 * time.Millisecond

var serialConfig *serial.Config
var serialPort *serial.Port

var readyToInitGPS bool //TODO: replace with channel control to terminate goroutine when complete

var Satellites map[string]SatelliteInfo

var ognTrackerConfigured = false;

/*
u-blox5_Referenzmanual.pdf
Platform settings
Airborne <2g Recommended for typical airborne environment. No 2D position fixes supported.
p.91 - CFG-MSG
Navigation/Measurement Rate Settings
Header 0xB5 0x62
ID 0x06 0x08
0x0064 (100 ms)
0x0001
0x0001 (GPS time)
{0xB5, 0x62, 0x06, 0x08, 0x00, 0x64, 0x00, 0x01, 0x00, 0x01}
p.109 CFG-NAV5 (0x06 0x24)
Poll Navigation Engine Settings
*/

/*
	chksumUBX()
		returns the two-byte Fletcher algorithm checksum of byte array msg.
		This is used in configuration messages for the u-blox GPS. See p. 97 of the
		u-blox M8 Receiver Description.
*/

func chksumUBX(msg []byte) []byte {
	ret := make([]byte, 2)
	for i := 0; i < len(msg); i++ {
		ret[0] = ret[0] + msg[i]
		ret[1] = ret[1] + ret[0]
	}
	return ret
}

/*
	makeUBXCFG()
		creates a UBX-formatted package consisting of two sync characters,
		class, ID, payload length in bytes (2-byte little endian), payload, and checksum.
		See p. 95 of the u-blox M8 Receiver Description.
*/
func makeUBXCFG(class, id byte, msglen uint16, msg []byte) []byte {
	ret := make([]byte, 6)
	ret[0] = 0xB5
	ret[1] = 0x62
	ret[2] = class
	ret[3] = id
	ret[4] = byte(msglen & 0xFF)
	ret[5] = byte((msglen >> 8) & 0xFF)
	ret = append(ret, msg...)
	chk := chksumUBX(ret[2:])
	ret = append(ret, chk[0])
	ret = append(ret, chk[1])
	return ret
}

func makeNMEACmd(cmd string) []byte {
	chk_sum := byte(0)
	for i := range cmd {
		chk_sum = chk_sum ^ byte(cmd[i])
	}
	return []byte(fmt.Sprintf("$%s*%02x\x0d\x0a", cmd, chk_sum))
}


func initGPSSerial() bool {
	var device string
	if (globalStatus.GPS_detected_type & 0x0f) == GPS_TYPE_NETWORK {
		return true
	}
	// Possible baud rates for this device. We will try to auto detect the correct one
	baudrates := []int{int(9600)}
	isSirfIV := bool(false)
	ognTrackerConfigured = false;
	globalStatus.GPS_detected_type = 0 // reset detected type on each initialization

	if _, err := os.Stat("/dev/ublox9"); err == nil { // u-blox 8 (RY83xAI over USB).
		device = "/dev/ublox9"
		globalStatus.GPS_detected_type = GPS_TYPE_UBX9
	} else if _, err := os.Stat("/dev/ublox8"); err == nil { // u-blox 8 (RY83xAI or GPYes 2.0).
		device = "/dev/ublox8"
		globalStatus.GPS_detected_type = GPS_TYPE_UBX8
		gpsTimeOffsetPpsMs = 80 * time.Millisecond // Ublox 8 seems to have higher delay
	} else if _, err := os.Stat("/dev/ublox7"); err == nil { // u-blox 7 (VK-172, VK-162 Rev 2, GPYes, RY725AI over USB).
		device = "/dev/ublox7"
		globalStatus.GPS_detected_type = GPS_TYPE_UBX7
	} else if _, err := os.Stat("/dev/ublox6"); err == nil { // u-blox 6 (VK-162 Rev 1).
		device = "/dev/ublox6"
		globalStatus.GPS_detected_type = GPS_TYPE_UBX6
	} else if _, err := os.Stat("/dev/prolific0"); err == nil { // Assume it's a BU-353-S4 SIRF IV.
		//TODO: Check a "serialout" flag and/or deal with multiple prolific devices.
		isSirfIV = true
		baudrates[0] = 4800
		device = "/dev/prolific0"
		globalStatus.GPS_detected_type = GPS_TYPE_PROLIFIC
	} else if _, err := os.Stat("/dev/serialin"); err == nil {
		device = "/dev/serialin"
		globalStatus.GPS_detected_type = GPS_TYPE_SERIAL
		// OGN Tracker uses 115200, SoftRF 38400
		baudrates = []int{115200, 38400, 9600}
 	} else if _, err := os.Stat("/dev/softrf_dongle"); err == nil {
		device = "/dev/softrf_dongle"
		globalStatus.GPS_detected_type = GPS_TYPE_SOFTRF_DONGLE
		baudrates[0] = 115200
 	} else if _, err := os.Stat("/dev/ttyAMA0"); err == nil { // ttyAMA0 is PL011 UART (GPIO pins 8 and 10) on all RPi.
		device = "/dev/ttyAMA0"
		globalStatus.GPS_detected_type = GPS_TYPE_UART
	} else {
		if globalSettings.DEBUG {
			log.Printf("No GPS device found.\n")
		}
		return false
	}
	if globalSettings.DEBUG {
		log.Printf("Using %s for GPS\n", device)
	}

	// Open port at default baud for config.
	serialConfig = &serial.Config{Name: device, Baud: baudrates[0]}
	p, err := serial.OpenPort(serialConfig)
	if err != nil {
		log.Printf("serial port err: %s\n", err.Error())
		return false
	}

	if isSirfIV {
		log.Printf("Using SiRFIV config.\n")
		// Enable 38400 baud.
		p.Write(makeNMEACmd("PSRF100,1,38400,8,1,0"))
		baudrates[0] = 38400
		p.Close()

		time.Sleep(250 * time.Millisecond)
		// Re-open port at newly configured baud so we can configure 5Hz messages.
		serialConfig = &serial.Config{Name: device, Baud: baudrates[0]}
		p, err = serial.OpenPort(serialConfig)

		// Enable 5Hz. (To switch back to 1Hz: $PSRF103,00,7,00,0*22)
		p.Write(makeNMEACmd("PSRF103,00,6,00,0"))

		// Enable GGA.
		p.Write(makeNMEACmd("PSRF103,00,00,01,01"))
		// Enable GSA.
		p.Write(makeNMEACmd("PSRF103,02,00,01,01"))
		// Enable RMC.
		p.Write(makeNMEACmd("PSRF103,04,00,01,01"))
		// Enable VTG.
		p.Write(makeNMEACmd("PSRF103,05,00,01,01"))
		// Enable GSV (once every 5 position updates)
		p.Write(makeNMEACmd("PSRF103,03,00,05,01"))

		if globalSettings.DEBUG {
			log.Printf("Finished writing SiRF GPS config to %s. Opening port to test connection.\n", device)
		}
	} else if globalStatus.GPS_detected_type == GPS_TYPE_UBX6 || globalStatus.GPS_detected_type == GPS_TYPE_UBX7 ||
	          globalStatus.GPS_detected_type == GPS_TYPE_UBX8 || globalStatus.GPS_detected_type == GPS_TYPE_UBX9 {

		// Byte order for UBX configuration is little endian.

		// GNSS configuration CFG-GNSS for ublox 7 and higher, p. 125 (v8)

		// Notes: ublox8 is multi-GNSS capable (simultaneous decoding of GPS and GLONASS, or
		// GPS and Galileo) if SBAS (e.g. WAAS) is unavailable. This may provide robustness
		// against jamming / interference on one set of frequencies. However, this will drop the
		// position reporting rate to 5 Hz during times multi-GNSS is in use. This shouldn't affect
		// gpsattitude too much --  without WAAS corrections, the algorithm could get jumpy at higher
		// sampling rates.

		// load default configuration             |      clearMask     |  |     saveMask       |  |     loadMask       |  deviceMask
		//p.Write(makeUBXCFG(0x06, 0x09, 13, []byte{0xFF, 0xFF, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0x00, 0x00, 0x03}))
		//time.Sleep(100* time.Millisecond) // pause and wait for the GPS to finish configuring itself before closing / reopening the port


		if globalStatus.GPS_detected_type == GPS_TYPE_UBX9 {
			if globalSettings.DEBUG {
				log.Printf("ublox 9 detected\n")
			}
			// ublox 9
			writeUblox9ConfigCommands(p)		
		} else if (globalStatus.GPS_detected_type == GPS_TYPE_UBX8) || (globalStatus.GPS_detected_type == GPS_TYPE_UART) { // assume that any GPS connected to serial GPIO is ublox8 (RY835/6AI)
			if globalSettings.DEBUG {
				log.Printf("ublox 8 detected\n")
			}
			// ublox 8
			writeUblox8ConfigCommands(p)
		} else if (globalStatus.GPS_detected_type == GPS_TYPE_UBX7) || (globalStatus.GPS_detected_type == GPS_TYPE_UBX6) {
			if globalSettings.DEBUG {
				log.Printf("ublox 6 or 7 detected\n")
			}
			// ublox 6,7
			cfgGnss := []byte{0x00, 0x00, 0xFF, 0x04} // numTrkChUse=0xFF: number of tracking channels to use will be set to number of tracking channels available in hardware
			gps     := []byte{0x00, 0x04, 0xFF, 0x00, 0x01, 0x00, 0x01, 0x01} // enable GPS with 4-255 channels (ublox default)
			sbas    := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01} // enable SBAS with 1-3 channels (ublox default)
			qzss    := []byte{0x05, 0x00, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01} // enable QZSS with 0-3 channel (ublox default)
			glonass := []byte{0x06, 0x08, 0xFF, 0x00, 0x00, 0x00, 0x01, 0x01} // disable GLONASS (ublox default)
			cfgGnss = append(cfgGnss, gps...)
			cfgGnss = append(cfgGnss, sbas...)
			cfgGnss = append(cfgGnss, qzss...)
			cfgGnss = append(cfgGnss, glonass...)
			p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss))
		}

		writeUbloxGenericCommands(10, p)

		// Reconfigure serial port.
		cfg := make([]byte, 20)
		cfg[0] = 0x01 // portID.
		cfg[1] = 0x00 // res0.
		cfg[2] = 0x00 // res1.
		cfg[3] = 0x00 // res1.

			
		//      [   7   ] [   6   ] [   5   ] [   4   ]
		//	0000 0000 0000 0000 0000 10x0 1100 0000
		// UART mode. 0 stop bits, no parity, 8 data bits. Little endian order.
		cfg[4] = 0xC0
		cfg[5] = 0x08
		cfg[6] = 0x00
		cfg[7] = 0x00

		// Baud rate. Little endian order.
		bdrt := uint32(115200)
		cfg[11] = byte((bdrt >> 24) & 0xFF)
		cfg[10] = byte((bdrt >> 16) & 0xFF)
		cfg[9] = byte((bdrt >> 8) & 0xFF)
		cfg[8] = byte(bdrt & 0xFF)

		// inProtoMask. NMEA and UBX. Little endian.
		cfg[12] = 0x03
		cfg[13] = 0x00

		// outProtoMask. NMEA. Little endian.
		cfg[14] = 0x02
		cfg[15] = 0x00

		cfg[16] = 0x00 // flags.
		cfg[17] = 0x00 // flags.

		cfg[18] = 0x00 //pad.
		cfg[19] = 0x00 //pad.

		// UBX-CFG-PRT (Port Configuration for UART)
		p.Write(makeUBXCFG(0x06, 0x00, 20, cfg))


		//	time.Sleep(100* time.Millisecond) // pause and wait for the GPS to finish configuring itself before closing / reopening the port
		baudrates[0] = int(bdrt)

		if globalSettings.DEBUG {
			log.Printf("Finished writing u-blox GPS config to %s. Opening port to test connection.\n", device)
		}
	} else if globalStatus.GPS_detected_type == GPS_TYPE_SOFTRF_DONGLE {
		p.Write([]byte("@GNS 0x7\r\n")) // enable SBAS
		p.Flush()
		time.Sleep(250* time.Millisecond) // Otherwise second command doesn't seem to work?
		p.Write([]byte("@BSSL 0x2D\r\n")) // enable GNGSV
		p.Flush()
	}
	p.Close()

	time.Sleep(250 * time.Millisecond)

	// Re-open port at newly configured baud so we can read messages. ReadTimeout is set to keep from blocking the gpsSerialReader() on misconfigures or ttyAMA disconnects
	// serialConfig = &serial.Config{Name: device, Baud: baudrate, ReadTimeout: time.Millisecond * 2500}
	// serial.OpenPort(serialConfig)
	p, err = detectOpenSerialPort(device, baudrates)
	if err != nil {
		log.Printf("serial port err: %s\n", err.Error())
		return false
	}

	serialPort = p
	return true
}

func detectOpenSerialPort(device string, baudrates []int) (*(serial.Port), error) {
	if len(baudrates) == 1 {
		serialConfig := &serial.Config{Name: device, Baud: baudrates[0], ReadTimeout: time.Millisecond * 2500}
		return serial.OpenPort(serialConfig)
	} else {
		for _, baud := range baudrates {
			serialConfig := &serial.Config{Name: device, Baud: baud, ReadTimeout: time.Millisecond * 2500}
			p, err := serial.OpenPort(serialConfig)
			if err != nil {
				return p, err
			}
			// Check if we get any data...
			time.Sleep(3 * time.Second)
			buffer := make([]byte, 10000)
			p.Read(buffer)
			splitted := strings.Split(string(buffer), "\n")
			for _, line := range splitted {
				_, validNMEAcs := validateNMEAChecksum(line)
				if validNMEAcs {
					// looks a lot like NMEA.. use it
					log.Printf("Detected serial port %s with baud %d", device, baud)
					// Make sure the NMEA is immediately parsed once, so updateStatus() doesn't see the GPS as disconnected before
					// first msg arrives
					processNMEALine(line)
					return p, nil
				}
			}
			p.Close()
			time.Sleep(250 * time.Millisecond)
		}
		return nil, errors.New("Failed to detect GPS serial baud rate")
	}
}

func writeUblox8ConfigCommands(p *serial.Port) {
	cfgGnss := []byte{0x00, 0x00, 0xFF, 0x05} // numTrkChUse=0xFF: number of tracking channels to use will be set to number of tracking channels available in hardware
	gps     := []byte{0x00, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable GPS with 8-16 channels (ublox default)
	sbas    := []byte{0x01, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01} // enable SBAS with 1-3 channels (ublox default)
	galileo := []byte{0x02, 0x08, 0x08, 0x00, 0x01, 0x00, 0x01, 0x01} // enable Galileo with 8-8 channels (ublox default: disabled and 4-8 channels)
	beidou  := []byte{0x03, 0x08, 0x10, 0x00, 0x00, 0x00, 0x01, 0x01} // disable BEIDOU
	qzss    := []byte{0x05, 0x01, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01} // enable QZSS 1-3 channels, L1C/A (ublox default: 0-3 channels)
	glonass := []byte{0x06, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable GLONASS with 8-16 channels (ublox default: 8-14 channels)
	
	cfgGnss = append(cfgGnss, gps...)
	cfgGnss = append(cfgGnss, sbas...)
	cfgGnss = append(cfgGnss, beidou...)
	cfgGnss = append(cfgGnss, qzss...)
	cfgGnss = append(cfgGnss, glonass...)
	p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss)) // Succeeds on all chips supporting GPS+GLO

	cfgGnss[3] = 0x06
	cfgGnss = append(cfgGnss, galileo...)
	p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss)) // Succeeds only on chips that support GPS+GLO+GAL
}

func writeUblox9ConfigCommands(p *serial.Port) {
	cfgGnss := []byte{0x00, 0x00, 0xFF, 0x06} // numTrkChUse=0xFF: number of tracking channels to use will be set to number of tracking channels available in hardware
	gps     := []byte{0x00, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable GPS with 8-16 channels (ublox default)
	sbas    := []byte{0x01, 0x03, 0x03, 0x00, 0x01, 0x00, 0x01, 0x01} // enable SBAS with 3-3 channels (ublox default)
	galileo := []byte{0x02, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable Galileo with 8-16 channels (ublox default: 8-12 channels)
	beidou  := []byte{0x03, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable BEIDOU with 8-16 channels (ublox default: 2-5 channels)
	qzss    := []byte{0x05, 0x03, 0x04, 0x00, 0x01, 0x00, 0x05, 0x01} // enable QZSS 3-4 channels, L1C/A & L1S (ublox default)
	glonass := []byte{0x06, 0x08, 0x10, 0x00, 0x01, 0x00, 0x01, 0x01} // enable GLONASS with 8-16 tracking channels (ublox default: 8-12 channels)
	
	cfgGnss = append(cfgGnss, gps...)
	cfgGnss = append(cfgGnss, sbas...)
	cfgGnss = append(cfgGnss, beidou...)
	cfgGnss = append(cfgGnss, qzss...)
	cfgGnss = append(cfgGnss, glonass...)
	cfgGnss = append(cfgGnss, galileo...)
	p.Write(makeUBXCFG(0x06, 0x3E, uint16(len(cfgGnss)), cfgGnss))
}

func writeUbloxGenericCommands(navrate uint16, p *serial.Port) {
	// UBX-CFG-NMEA (change NMEA protocol version to 4.0 extended)
	p.Write(makeUBXCFG(0x06, 0x17, 20, []byte{0x00, 0x40, 0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}))

	// UBX-CFG-PMS
	p.Write(makeUBXCFG(0x06, 0x86, 8, []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Full Power Mode
	// p.Write(makeUBXCFG(0x06, 0x86, 8, []byte{0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Balanced Power Mode

	// UBX-CFG-NAV5                           |mask1...|  dyn
	p.Write(makeUBXCFG(0x06, 0x24, 36, []byte{0x01, 0x00, 0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Dynamic platform model: airborne with <2g acceleration

	// UBX-CFG-SBAS (disable integrity, enable auto-scan)
	p.Write(makeUBXCFG(0x06, 0x16, 8, []byte{0x01, 0x03, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00}))

	// UBX-CFG-MSG (NMEA Standard Messages)  msg   msg   Ports 1-6 (every 10th message over UART1, every message over USB)
	//                                       Class ID    DDC   UART1 UART2 USB   I2C   Res
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})) // GGA - Global positioning system fix data
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GLL - Latitude and longitude, with time of position fix and status
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x02, 0x00, 0x05, 0x00, 0x05, 0x00, 0x00})) // GSA - GNSS DOP and Active Satellites
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x03, 0x00, 0x05, 0x00, 0x05, 0x00, 0x00})) // GSV - GNSS Satellites in View
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x04, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})) // RMC - Recommended Minimum data
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x05, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00})) // VGT - Course over ground and Ground speed
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GRS - GNSS Range Residuals
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x07, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GST - GNSS Pseudo Range Error Statistics
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // ZDA - Time and Date<
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x09, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GBS - GNSS Satellite Fault Detection
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0A, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // DTM - Datum Reference
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0D, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // GNS - GNSS fix data
	// p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0E, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // ???
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF0, 0x0F, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // VLW - Dual ground/water distance

	// UBX-CFG-MSG (NMEA PUBX Messages)      msg   msg   Ports 1-6
	//                                       Class ID    DDC   UART1 UART2 USB   I2C   Res
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF1, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Ublox - Lat/Long Position Data
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF1, 0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Ublox - Satellite Status
	p.Write(makeUBXCFG(0x06, 0x01, 8, []byte{0xF1, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})) // Ublox - Time of Day and Clock Information



	if navrate == 10 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0x64, 0x00, 0x01, 0x00, 0x01, 0x00})) // 100ms & 1 cycle -> 10Hz (UBX-CFG-RATE payload bytes: little endian!)
	} else if navrate == 5 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0xC8, 0x00, 0x01, 0x00, 0x01, 0x00})) // 200ms & 1 cycle -> 5Hz (UBX-CFG-RATE payload bytes: little endian!)
	} else if navrate == 2 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0xF4, 0x01, 0x01, 0x00, 0x01, 0x00})) // 500ms & 1 cycle -> 2Hz (UBX-CFG-RATE payload bytes: little endian!)
	} else if navrate == 1 {
		p.Write(makeUBXCFG(0x06, 0x08, 6, []byte{0xE8, 0x03, 0x01, 0x00, 0x01, 0x00})) // 1000ms & 1 cycle -> 1Hz (UBX-CFG-RATE payload bytes: little endian!)
	}


}


func configureOgnTracker() {
	if serialPort == nil {
		return
	}

	gpsTimeOffsetPpsMs = 200 * time.Millisecond
	serialPort.Write([]byte("$POGNS,NavRate=5\r\n")) // Also force NavRate directly, just to make sure it's always set
	serialPort.Write([]byte("$POGNS\r\n")) // query current configuration

	// Configuration for OGN Tracker T-Beam is similar to normal Ublox config, but

	writeUblox8ConfigCommands(serialPort)
	writeUbloxGenericCommands(5, serialPort)

	serialPort.Flush()

	globalStatus.GPS_detected_type = GPS_TYPE_OGNTRACKER
}

// func validateNMEAChecksum determines if a string is a properly formatted NMEA sentence with a valid checksum.
//
// If the input string is valid, output is the input stripped of the "$" token and checksum, along with a boolean 'true'
// If the input string is the incorrect format, the checksum is missing/invalid, or checksum calculation fails, an error string and
// boolean 'false' are returned
//
// Checksum is calculated as XOR of all bytes between "$" and "*"

func validateNMEAChecksum(s string) (string, bool) {
	//validate format. NMEA sentences start with "$" and end in "*xx" where xx is the XOR value of all bytes between
	if !(strings.HasPrefix(s, "$") && strings.Contains(s, "*")) {
		return "", false
	}

	// strip leading "$" and split at "*"
	s_split := strings.Split(strings.TrimPrefix(s, "$"), "*")
	s_out := s_split[0]
	s_cs := s_split[1]

	if len(s_cs) < 2 {
		return "Missing checksum. Fewer than two bytes after asterisk", false
	}

	cs, err := strconv.ParseUint(s_cs[:2], 16, 8)
	if err != nil {
		return "Invalid checksum", false
	}

	cs_calc := byte(0)
	for i := range s_out {
		cs_calc = cs_calc ^ byte(s_out[i])
	}

	if cs_calc != byte(cs) {
		return fmt.Sprintf("Checksum failed. Calculated %#X; expected %#X", cs_calc, cs), false
	}

	return s_out, true
}

//  Only count this heading if a "sustained" >7 kts is obtained. This filters out a lot of heading
//  changes while on the ground and "movement" is really only changes in GPS fix as it settles down.
//TODO: Some more robust checking above current and last speed.
//TODO: Dynamic adjust for gain based on groundspeed
func setTrueCourse(groundSpeed uint16, trueCourse float64) {
	if mySituation.GPSGroundSpeed >= 7 && groundSpeed >= 7 {
		// This was previously used to filter small ground speed spikes caused by GPS position drift.
		//  It was passed to the previous AHRS heading calculator. Currently unused, maybe in the future it will be.
		_ = trueCourse
		_ = groundSpeed
	}
}

/*
calcGPSAttitude estimates turn rate, pitch, and roll based on recent GPS groundspeed, track, and altitude / vertical speed.

Method uses stored performance statistics from myGPSPerfStats[]. Ideally, calculation is based on most recent 1.5 seconds of data,
assuming 10 Hz sampling frequency. Lower frequency sample rates will increase calculation window for smoother response, at the
cost of slightly increased lag.

(c) 2016 Keith Tschohl. All rights reserved.
Distributable under the terms of the "BSD-New" License that can be found in
the LICENSE file, herein included as part of this header.
*/

func calcGPSAttitude() bool {
	// check slice length. Return error if empty set or set zero values
	mySituation.muGPSPerformance.Lock()
	defer mySituation.muGPSPerformance.Unlock()
	length := len(myGPSPerfStats)
	index := length - 1

	if length == 0 {
		log.Printf("GPS attitude: No data received yet. Not calculating attitude.\n")
		return false
	} else if length == 1 {
		//log.Printf("myGPSPerfStats has one data point. Setting statistics to zero.\n")
		myGPSPerfStats[index].gpsTurnRate = 0
		myGPSPerfStats[index].gpsPitch = 0
		myGPSPerfStats[index].gpsRoll = 0
		return false
	}

	// check if GPS data was put in the structure more than three seconds ago -- this shouldn't happen unless something is wrong.
	if (stratuxClock.Milliseconds - myGPSPerfStats[index].stratuxTime) > 3000 {
		myGPSPerfStats[index].gpsTurnRate = 0
		myGPSPerfStats[index].gpsPitch = 0
		myGPSPerfStats[index].gpsRoll = 0
		log.Printf("GPS attitude: GPS data is more than three seconds old. Setting attitude to zero.\n")
		return false
	}

	// check time interval between samples
	t1 := myGPSPerfStats[index].nmeaTime
	t0 := myGPSPerfStats[index-1].nmeaTime
	dt := t1 - t0

	// first time error case: index is more than three seconds ahead of index-1
	if dt > 3 {
		log.Printf("GPS attitude: Can't calculate GPS attitude. Reference data is old. dt = %v\n", dt)
		return false
	}

	// second case: index is behind index-1. This could be result of day rollover. If time is within n seconds of UTC,
	// we rebase to the previous day, and will re-rebase the entire slice forward to the current day once all values roll over.
	//TODO: Validate by testing at 0000Z
	if dt < 0 {
		log.Printf("GPS attitude: Current GPS time (%.2f) is older than last GPS time (%.2f). Checking for 0000Z rollover.\n", t1, t0)
		if myGPSPerfStats[index-1].nmeaTime > 86300 && myGPSPerfStats[index].nmeaTime < 100 { // be generous with the time window at rollover
			myGPSPerfStats[index].nmeaTime += 86400
		} else {
			// time decreased, but not due to a recent rollover. Something odd is going on.
			log.Printf("GPS attitude: Time isn't near 0000Z. Unknown reason for offset. Can't calculate GPS attitude.\n")
			return false
		}

		// check time array to see if all timestamps are > 86401 seconds since midnight
		var tempTime []float64
		tempTime = make([]float64, length, length)
		for i := 0; i < length; i++ {
			tempTime[i] = float64(myGPSPerfStats[i].nmeaTime)
		}
		minTime, _ := arrayMin(tempTime)
		if minTime > 86401.0 {
			log.Printf("GPS attitude: Rebasing GPS time since midnight to current day.\n")
			for i := 0; i < length; i++ {
				myGPSPerfStats[i].nmeaTime -= 86400
			}
		}

		// Verify adjustment
		dt = myGPSPerfStats[index].nmeaTime - myGPSPerfStats[index-1].nmeaTime
		log.Printf("GPS attitude: New dt = %f\n", dt)
		if dt > 3 {
			log.Printf("GPS attitude: Can't calculate GPS attitude. Reference data is old. dt = %v\n", dt)
			return false
		} else if dt < 0 {
			log.Printf("GPS attitude: Something went wrong rebasing the time.\n")
			return false
		}

	}

	// If all of the bounds checks pass, begin processing the GPS data.

	// local variables
	var headingAvg, dh, v_x, v_z, a_c, omega, slope, intercept float64
	var tempHdg, tempHdgUnwrapped, tempHdgTime, tempSpeed, tempVV, tempSpeedTime, tempRegWeights []float64 // temporary arrays for regression calculation
	var valid bool
	var lengthHeading, lengthSpeed int
	var halfwidth float64 // width of regression evaluation window. Minimum of 1.5 seconds and maximum of 3.5 seconds.

	center := float64(myGPSPerfStats[index].nmeaTime) // current time for calculating regression weights

/*	// frequency detection
	tempSpeedTime = make([]float64, 0)
	for i := 1; i < length; i++ {
		dt = myGPSPerfStats[i].nmeaTime - myGPSPerfStats[i-1].nmeaTime
		if dt > 0.05 { // avoid double counting messages with same / similar timestamps
			tempSpeedTime = append(tempSpeedTime, float64(dt))
		}
	}
	//log.Printf("Delta time array is %v.\n",tempSpeedTime)
	dt_avg, valid = mean(tempSpeedTime)
	if valid && dt_avg > 0 {
		if globalSettings.DEBUG {
			log.Printf("GPS attitude: Average delta time is %.2f s (%.1f Hz)\n", dt_avg, 1/dt_avg)
		}
		halfwidth = 9 * dt_avg
		mySituation.GPSPositionSampleRate = 1 / dt_avg
	} else {
		if globalSettings.DEBUG {
			log.Printf("GPS attitude: Couldn't determine sample rate\n")
		}
		halfwidth = 3.5
		mySituation.GPSPositionSampleRate = 0
	}

	if halfwidth > 3.5 {
		halfwidth = 3.5 // limit calculation window to 3.5 seconds of data for 1 Hz or slower samples
	} else if halfwidth < 1.5 {
		halfwidth = 1.5 // use minimum of 1.5 seconds for sample rates faster than 5 Hz
	}
*/
	halfwidth = calculateNavRate()

	//v_x = float64(myGPSPerfStats[index].gsf * 1.687810)
	//v_z = 0

	// first, parse groundspeed from RMC messages.
	tempSpeedTime = make([]float64, 0)
	tempSpeed = make([]float64, 0)
	tempRegWeights = make([]float64, 0)

	for i := 0; i < length; i++ {
		if myGPSPerfStats[i].msgType == "GPRMC" || myGPSPerfStats[i].msgType == "GNRMC" {
			tempSpeed = append(tempSpeed, float64(myGPSPerfStats[i].gsf))
			tempSpeedTime = append(tempSpeedTime, float64(myGPSPerfStats[i].nmeaTime))
			tempRegWeights = append(tempRegWeights, triCubeWeight(center, halfwidth, float64(myGPSPerfStats[i].nmeaTime)))
		}
	}
	lengthSpeed = len(tempSpeed)
	if lengthSpeed == 0 {
		log.Printf("GPS Attitude: No groundspeed data could be parsed from NMEA RMC messages\n")
		return false
	} else if lengthSpeed == 1 {
		v_x = tempSpeed[0] * 1.687810
	} else {
		slope, intercept, valid = linRegWeighted(tempSpeedTime, tempSpeed, tempRegWeights)
		if !valid {
			log.Printf("GPS attitude: Error calculating speed regression from NMEA RMC position messages")
			return false
		} else {
			v_x = (slope*float64(myGPSPerfStats[index].nmeaTime) + intercept) * 1.687810 // units are knots, converted to feet/sec
			//log.Printf("Avg speed %f calculated from %d RMC messages\n", v_x, lengthSpeed) // DEBUG
		}
	}

	// next, calculate vertical velocity from GGA altitude data.
	tempSpeedTime = make([]float64, 0)
	tempVV = make([]float64, 0)
	tempRegWeights = make([]float64, 0)

	for i := 0; i < length; i++ {
		if myGPSPerfStats[i].msgType == "GPGGA" || myGPSPerfStats[i].msgType == "GNGGA" {
			tempVV = append(tempVV, float64(myGPSPerfStats[i].alt))
			tempSpeedTime = append(tempSpeedTime, float64(myGPSPerfStats[i].nmeaTime))
			tempRegWeights = append(tempRegWeights, triCubeWeight(center, halfwidth, float64(myGPSPerfStats[i].nmeaTime)))
		}
	}
	lengthSpeed = len(tempVV)
	if lengthSpeed < 2 {
		log.Printf("GPS Attitude: Not enough points to calculate vertical speed from NMEA GGA messages\n")
		return false
	} else {
		slope, _, valid = linRegWeighted(tempSpeedTime, tempVV, tempRegWeights)
		if !valid {
			log.Printf("GPS attitude: Error calculating vertical speed regression from NMEA GGA messages")
			return false
		} else {
			v_z = slope // units are feet/sec
			//log.Printf("Avg VV %f calculated from %d GGA messages\n", v_z, lengthSpeed) // DEBUG
		}
	}

	// If we're going too slow for processNMEALine() to give us valid heading data, there's no sense in trying to parse it.
	// However, we need to return a valid level attitude so we don't get the "red X of death" on our AHRS display.
	// This will also eliminate most of the nuisance error message from the turn rate calculation.
	if v_x < 6 { // ~3.55 knots

		myGPSPerfStats[index].gpsPitch = 0
		myGPSPerfStats[index].gpsRoll = 0
		myGPSPerfStats[index].gpsTurnRate = 0
		myGPSPerfStats[index].gpsLoadFactor = 1.0
		mySituation.GPSTurnRate = 0

		// Output format:GPSAtttiude,seconds,nmeaTime,msg_type,GS,Course,Alt,VV,filtered_GS,filtered_course,turn rate,filtered_vv,pitch, roll,load_factor
		buf := fmt.Sprintf("GPSAttitude,%.1f,%.2f,%s,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f\n", float64(stratuxClock.Milliseconds)/1000, myGPSPerfStats[index].nmeaTime, myGPSPerfStats[index].msgType, myGPSPerfStats[index].gsf, myGPSPerfStats[index].coursef, myGPSPerfStats[index].alt, myGPSPerfStats[index].vv, v_x/1.687810, headingAvg, myGPSPerfStats[index].gpsTurnRate, v_z, myGPSPerfStats[index].gpsPitch, myGPSPerfStats[index].gpsRoll, myGPSPerfStats[index].gpsLoadFactor)
		if globalSettings.DEBUG {
			log.Printf("%s", buf) // FIXME. Send to sqlite log or other file?
		}
		logGPSAttitude(myGPSPerfStats[index])
		//replayLog(buf, MSGCLASS_AHRS)

		return true
	}

	// Heading.  Same method used for UBX and generic.
	// First, walk through the PerfStats and parse only valid heading data.
	//log.Printf("Raw heading data:")
	for i := 0; i < length; i++ {
		//log.Printf("%.1f,",myGPSPerfStats[i].coursef)
		if myGPSPerfStats[i].coursef >= 0 { // negative values are used to flag invalid / unavailable course
			tempHdg = append(tempHdg, float64(myGPSPerfStats[i].coursef))
			tempHdgTime = append(tempHdgTime, float64(myGPSPerfStats[i].nmeaTime))
		}
	}
	//log.Printf("\n")
	//log.Printf("tempHdg: %v\n", tempHdg)

	// Next, unwrap the heading so we don't mess up the regression by fitting a line across the 0/360 deg discontinuity.
	lengthHeading = len(tempHdg)
	tempHdgUnwrapped = make([]float64, lengthHeading, lengthHeading)
	tempRegWeights = make([]float64, lengthHeading, lengthHeading)

	if lengthHeading > 1 {
		tempHdgUnwrapped[0] = tempHdg[0]
		tempRegWeights[0] = triCubeWeight(center, halfwidth, tempHdgTime[0])
		for i := 1; i < lengthHeading; i++ {
			tempRegWeights[i] = triCubeWeight(center, halfwidth, tempHdgTime[i])
			if math.Abs(tempHdg[i]-tempHdg[i-1]) < 180 { // case 1: if angle change is less than 180 degrees, use the same reference system
				tempHdgUnwrapped[i] = tempHdgUnwrapped[i-1] + tempHdg[i] - tempHdg[i-1]
			} else if tempHdg[i] > tempHdg[i-1] { // case 2: heading has wrapped around from NE to NW. Subtract 360 to keep consistent with previous data.
				tempHdgUnwrapped[i] = tempHdgUnwrapped[i-1] + tempHdg[i] - tempHdg[i-1] - 360
			} else { // case 3:  heading has wrapped around from NW to NE. Add 360 to keep consistent with previous data.
				tempHdgUnwrapped[i] = tempHdgUnwrapped[i-1] + tempHdg[i] - tempHdg[i-1] + 360
			}
		}
	} else { //
		if globalSettings.DEBUG {
			log.Printf("GPS attitude: Can't calculate turn rate with less than two points.\n")
		}
		return false
	}

	// Finally, calculate turn rate as the slope of the weighted linear regression of unwrapped heading.
	slope, intercept, valid = linRegWeighted(tempHdgTime, tempHdgUnwrapped, tempRegWeights)

	if !valid {
		log.Printf("GPS attitude: Regression error calculating turn rate")
		return false
	} else {
		headingAvg = slope*float64(myGPSPerfStats[index].nmeaTime) + intercept
		dh = slope // units are deg per sec; no conversion needed here
		//log.Printf("Calculated heading and turn rate: %.3f degrees, %.3f deg/sec\n",headingAvg,dh)
	}

	myGPSPerfStats[index].gpsTurnRate = dh
	mySituation.GPSTurnRate = dh

	// pitch angle -- or to be more pedantic, glide / climb angle, since we're just looking a rise-over-run.
	// roll angle, based on turn rate and ground speed. Only valid for coordinated flight. Differences between airspeed and groundspeed will trip this up.
	if v_x > 20 { // reduce nuisance 'bounce' at low speeds. 20 ft/sec = 11.9 knots.
		myGPSPerfStats[index].gpsPitch = math.Atan2(v_z, v_x) * 180.0 / math.Pi

		/*
			Governing equations for roll calculations

			Physics tells us that
				a_z = g     (in steady-state flight -- climbing, descending, or level -- this is gravity. 9.81 m/s^2 or 32.2 ft/s^2)
				a_c = v^2/r (centripetal acceleration)

			We don't know r. However, we do know the tangential velocity (v) and angular velocity (omega). Express omega in radians per unit time, and

				v = omega*r

			By substituting and rearranging terms:

				a_c = v^2 / (v / omega)
				a_c = v*omega

			Free body diagram time!

				   /|
			  a_r / |  a_z
				 /__|
			   X   a_c
				\_________________ [For the purpose of this comment, " X" is an airplane in a 20 degree bank. Use your imagination, mkay?)

			Resultant acceleration a_r is what the wings feel; a_r/a_z = load factor. Anyway, trig out the bank angle:

				bank angle = atan(a_c/a_z)
						   = atan(v*omega/g)

				wing loading = sqrt(a_c^2 + a_z^2) / g

		*/

		g := 32.174                                        // ft/(s^2)
		omega = radians(myGPSPerfStats[index].gpsTurnRate) // need radians/sec
		a_c = v_x * omega
		myGPSPerfStats[index].gpsRoll = math.Atan2(a_c, g) * 180 / math.Pi // output is degrees
		myGPSPerfStats[index].gpsLoadFactor = math.Sqrt(a_c*a_c+g*g) / g
	} else {
		myGPSPerfStats[index].gpsPitch = 0
		myGPSPerfStats[index].gpsRoll = 0
		myGPSPerfStats[index].gpsLoadFactor = 1
	}

	if globalSettings.DEBUG {
		// Output format:GPSAtttiude,seconds,nmeaTime,msg_type,GS,Course,Alt,VV,filtered_GS,filtered_course,turn rate,filtered_vv,pitch, roll,load_factor
		buf := fmt.Sprintf("GPSAttitude,%.1f,%.2f,%s,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f,%0.3f\n", float64(stratuxClock.Milliseconds)/1000, myGPSPerfStats[index].nmeaTime, myGPSPerfStats[index].msgType, myGPSPerfStats[index].gsf, myGPSPerfStats[index].coursef, myGPSPerfStats[index].alt, myGPSPerfStats[index].vv, v_x/1.687810, headingAvg, myGPSPerfStats[index].gpsTurnRate, v_z, myGPSPerfStats[index].gpsPitch, myGPSPerfStats[index].gpsRoll, myGPSPerfStats[index].gpsLoadFactor)
		log.Printf("%s", buf) // FIXME. Send to sqlite log or other file?
	}

	logGPSAttitude(myGPSPerfStats[index])
	//replayLog(buf, MSGCLASS_AHRS)
	return true
}

func calculateNACp(accuracy float32) uint8 {
	ret := uint8(0)

	if accuracy < 3 {
		ret = 11
	} else if accuracy < 10 {
		ret = 10
	} else if accuracy < 30 {
		ret = 9
	} else if accuracy < 92.6 {
		ret = 8
	} else if accuracy < 185.2 {
		ret = 7
	} else if accuracy < 555.6 {
		ret = 6
	}

	return ret
}

/*
	registerSituationUpdate().
	 Called whenever there is a change in mySituation.
*/
func registerSituationUpdate() {
	logSituation()
	situationUpdate.SendJSON(mySituation)
}

func calculateNavRate() float64 {
 	length := len(myGPSPerfStats)
 	tempSpeedTime := make([]float64, 0)

 	for i := 1; i < length; i++ {
 		dt := myGPSPerfStats[i].nmeaTime - myGPSPerfStats[i-1].nmeaTime
 		if dt > 0.05 { // avoid double counting messages with same / similar timestamps
 			tempSpeedTime = append(tempSpeedTime, float64(dt))
 		}
 	}

 	var halfwidth float64
 	dt_avg, valid := mean(tempSpeedTime)
 	if valid && dt_avg > 0 {
 		if globalSettings.DEBUG {
 			log.Printf("GPS attitude: Average delta time is %.2f s (%.1f Hz)\n", dt_avg, 1/dt_avg)
 		}
 		halfwidth = 9 * dt_avg
 		mySituation.GPSPositionSampleRate = 1 / dt_avg
 	} else {
 		if globalSettings.DEBUG {
 			log.Printf("GPS attitude: Couldn't determine sample rate\n")
 		}
 		halfwidth = 3.5
 		mySituation.GPSPositionSampleRate = 0
 	}

 	if halfwidth > 3.5 {
 		halfwidth = 3.5 // limit calculation window to 3.5 seconds of data for 1 Hz or slower samples
 	} else if halfwidth < 1.5 {
 		halfwidth = 1.5 // use minimum of 1.5 seconds for sample rates faster than 5 Hz
 	}

 	return halfwidth
 }

/*
processNMEALine parses NMEA-0183 formatted strings against several message types.

Standard messages supported: RMC GGA VTG GSA

return is false if errors occur during parse, or if GPS position is invalid
return is true if parse occurs correctly and position is valid.

*/

func processNMEALine(l string) (sentenceUsed bool) {
	mySituation.muGPS.Lock()

	defer func() {
		if sentenceUsed || globalSettings.DEBUG {
			registerSituationUpdate()
		}
		mySituation.muGPS.Unlock()
	}()

	// Local variables for GPS attitude estimation
	thisGpsPerf := gpsPerf                              // write to myGPSPerfStats at end of function IFF
	thisGpsPerf.coursef = -999.9                        // default value of -999.9 indicates invalid heading to regression calculation
	thisGpsPerf.stratuxTime = stratuxClock.Milliseconds // used for gross indexing
	updateGPSPerf := false                              // change to true when position or vector info is read

	l_valid, validNMEAcs := validateNMEAChecksum(l)
	if !validNMEAcs {
		if len(l_valid) > 0 {
			log.Printf("GPS error. Invalid NMEA string: %s\n", l_valid) // remove log message once validation complete
		}
		return false
	}
	ognPublishNmea(l)
	x := strings.Split(l_valid, ",")

	mySituation.GPSLastValidNMEAMessageTime = stratuxClock.Time
	mySituation.GPSLastValidNMEAMessage = l

	if (x[0] == "GNVTG") || (x[0] == "GPVTG") { // Ground track information.
		tmpSituation := mySituation // If we decide to not use the data in this message, then don't make incomplete changes in mySituation.
		if len(x) < 9 {             // Reduce from 10 to 9 to allow parsing by devices pre-NMEA v2.3
			return false
		}

		groundspeed, err := strconv.ParseFloat(x[5], 32) // Knots.
		if err != nil {
			return false
		}
		tmpSituation.GPSGroundSpeed = groundspeed

		trueCourse := float32(0)
		tc, err := strconv.ParseFloat(x[1], 32)
		if err != nil {
			return false
		}
		if groundspeed > 3 { //TODO: use average groundspeed over last n seconds to avoid random "jumps"
			trueCourse = float32(tc)
			setTrueCourse(uint16(groundspeed), tc)
			tmpSituation.GPSTrueCourse = trueCourse
		} else {
			// Negligible movement. Don't update course, but do use the slow speed.
			//TODO: use average course over last n seconds?
		}
		tmpSituation.GPSLastGroundTrackTime = stratuxClock.Time

		// We've made it this far, so that means we've processed "everything" and can now make the change to mySituation.
		mySituation = tmpSituation
		return true

	} else if (x[0] == "GNGGA") || (x[0] == "GPGGA") { // Position fix.
		tmpSituation := mySituation // If we decide to not use the data in this message, then don't make incomplete changes in mySituation.

		if len(x) < 15 {
			return false
		}

		// use RMC / GGA message detection to sense "NMEA" type.
		if (globalStatus.GPS_detected_type & 0xf0) == 0 {
			globalStatus.GPS_detected_type |= GPS_PROTOCOL_NMEA
		}

		// GPSFixQuality indicator.
		q, err1 := strconv.Atoi(x[6])
		if err1 != nil {
			return false
		}
		tmpSituation.GPSFixQuality = uint8(q) // 1 = 3D GPS; 2 = DGPS (SBAS /WAAS)

		// Timestamp.
		if len(x[1]) < 7 {
			return false
		}
		hr, err1 := strconv.Atoi(x[1][0:2])
		min, err2 := strconv.Atoi(x[1][2:4])
		sec, err3 := strconv.ParseFloat(x[1][4:], 32)
		if err1 != nil || err2 != nil || err3 != nil {
			return false
		}

		tmpSituation.GPSLastFixSinceMidnightUTC = float32(3600*hr+60*min) + float32(sec)
		thisGpsPerf.nmeaTime = tmpSituation.GPSLastFixSinceMidnightUTC

		// Latitude.
		if len(x[2]) < 4 {
			return false
		}

		hr, err1 = strconv.Atoi(x[2][0:2])
		minf, err2 := strconv.ParseFloat(x[2][2:], 32)
		if err1 != nil || err2 != nil {
			return false
		}

		tmpSituation.GPSLatitude = float32(hr) + float32(minf/60.0)
		if x[3] == "S" { // South = negative.
			tmpSituation.GPSLatitude = -tmpSituation.GPSLatitude
		}

		// Longitude.
		if len(x[4]) < 5 {
			return false
		}
		hr, err1 = strconv.Atoi(x[4][0:3])
		minf, err2 = strconv.ParseFloat(x[4][3:], 32)
		if err1 != nil || err2 != nil {
			return false
		}

		tmpSituation.GPSLongitude = float32(hr) + float32(minf/60.0)
		if x[5] == "W" { // West = negative.
			tmpSituation.GPSLongitude = -tmpSituation.GPSLongitude
		}

		// Altitude.
		alt, err1 := strconv.ParseFloat(x[9], 32)
		if err1 != nil {
			return false
		}
		tmpSituation.GPSAltitudeMSL = float32(alt * 3.28084) // Convert to feet.
		thisGpsPerf.alt = float32(tmpSituation.GPSAltitudeMSL)

		// Geoid separation (Sep = HAE - MSL)

		geoidSep, err1 := strconv.ParseFloat(x[11], 32)
		if err1 != nil {
			return false
		}
		tmpSituation.GPSGeoidSep = float32(geoidSep * 3.28084) // Convert to feet.
		tmpSituation.GPSHeightAboveEllipsoid = tmpSituation.GPSGeoidSep + tmpSituation.GPSAltitudeMSL

		// Timestamp.
		tmpSituation.GPSLastFixLocalTime = stratuxClock.Time

		updateGPSPerf = true
		thisGpsPerf.msgType = x[0]

		// We've made it this far, so that means we've processed "everything" and can now make the change to mySituation.
		mySituation = tmpSituation

		if updateGPSPerf {
			mySituation.muGPSPerformance.Lock()
			myGPSPerfStats = append(myGPSPerfStats, thisGpsPerf)
			lenGPSPerfStats := len(myGPSPerfStats)
			//	log.Printf("GPSPerf array has %n elements. Contents are: %v\n",lenGPSPerfStats,myGPSPerfStats)
			if lenGPSPerfStats > 299 { //30 seconds @ 10 Hz for UBX, 30 seconds @ 5 Hz for MTK or SIRF with 2x messages per 200 ms)
				myGPSPerfStats = myGPSPerfStats[(lenGPSPerfStats - 299):] // remove the first n entries if more than 300 in the slice
			}
			mySituation.muGPSPerformance.Unlock()
		}

		return true

	} else if (x[0] == "GNRMC") || (x[0] == "GPRMC") { // Recommended Minimum data.
		tmpSituation := mySituation // If we decide to not use the data in this message, then don't make incomplete changes in mySituation.

		//$GPRMC,123519,A,4807.038,N,01131.000,E,022.4,084.4,230394,003.1,W*6A
		/*						check RY835 man for NMEA version, if >2.2, add mode field
				Where:
		     RMC          Recommended Minimum sentence C
		     123519       Fix taken at 12:35:19 UTC
		     A            Status A=active or V=Void.
		     4807.038,N   Latitude 48 deg 07.038' N
		     01131.000,E  Longitude 11 deg 31.000' E
		     022.4        Speed over the ground in knots
		     084.4        Track angle in degrees True
		     230394       Date - 23rd of March 1994
		     003.1,W      Magnetic Variation
		     D				mode field (nmea 2.3 and higher)
		     *6A          The checksum data, always begins with *
		*/
		if len(x) < 11 {
			return false
		}

		// use RMC / GGA message detection to sense "NMEA" type.
		if (globalStatus.GPS_detected_type & 0xf0) == 0 {
			globalStatus.GPS_detected_type |= GPS_PROTOCOL_NMEA
		}

		if x[2] != "A" { // invalid fix
			tmpSituation.GPSFixQuality = 0 // Just a note.
			return false
		}

		// Timestamp.
		if len(x[1]) < 7 {
			return false
		}
		hr, err1 := strconv.Atoi(x[1][0:2])
		min, err2 := strconv.Atoi(x[1][2:4])
		sec, err3 := strconv.ParseFloat(x[1][4:], 32)
		if err1 != nil || err2 != nil || err3 != nil {
			return false
		}
		tmpSituation.GPSLastFixSinceMidnightUTC = float32(3600*hr+60*min) + float32(sec)
		thisGpsPerf.nmeaTime = tmpSituation.GPSLastFixSinceMidnightUTC

		if len(x[9]) == 6 {
			// Date of Fix, i.e 191115 =  19 November 2015 UTC  field 9
			gpsTimeStr := fmt.Sprintf("%s %02d:%02d:%06.3f", x[9], hr, min, sec)
			gpsTime, err := time.Parse("020106 15:04:05.000", gpsTimeStr)
			gpsTime = gpsTime.Add(gpsTimeOffsetPpsMs) // rough estimate for PPS offset
			if err == nil && gpsTime.After(time.Date(2016, time.January, 0, 0, 0, 0, 0, time.UTC)) { // Ignore dates before 2016-JAN-01.
				tmpSituation.GPSLastGPSTimeStratuxTime = stratuxClock.Time
				tmpSituation.GPSTime = gpsTime
				stratuxClock.SetRealTimeReference(gpsTime)
				if time.Since(gpsTime) > 200*time.Millisecond || time.Since(gpsTime) < -200*time.Millisecond {
					setStr := gpsTime.Format("20060102 15:04:05.000") + " UTC"
					log.Printf("setting system time from %s to: '%s'\n", time.Now().Format("20060102 15:04:05.000"), setStr)
					if err := exec.Command("date", "-s", setStr).Run(); err != nil {
						log.Printf("Set Date failure: %s error\n", err)
					} else {
						log.Printf("Time set from GPS. Current time is %v\n", time.Now())
					}
				}
			}
		}

		// Latitude.
		if len(x[3]) < 4 {
			return false
		}
		hr, err1 = strconv.Atoi(x[3][0:2])
		minf, err2 := strconv.ParseFloat(x[3][2:], 32)
		if err1 != nil || err2 != nil {
			return false
		}
		tmpSituation.GPSLatitude = float32(hr) + float32(minf/60.0)
		if x[4] == "S" { // South = negative.
			tmpSituation.GPSLatitude = -tmpSituation.GPSLatitude
		}
		// Longitude.
		if len(x[5]) < 5 {
			return false
		}
		hr, err1 = strconv.Atoi(x[5][0:3])
		minf, err2 = strconv.ParseFloat(x[5][3:], 32)
		if err1 != nil || err2 != nil {
			return false
		}
		tmpSituation.GPSLongitude = float32(hr) + float32(minf/60.0)
		if x[6] == "W" { // West = negative.
			tmpSituation.GPSLongitude = -tmpSituation.GPSLongitude
		}

		tmpSituation.GPSLastFixLocalTime = stratuxClock.Time

		// ground speed in kts (field 7)
		groundspeed, err := strconv.ParseFloat(x[7], 32)
		if err != nil {
			return false
		}
		tmpSituation.GPSGroundSpeed = groundspeed
		thisGpsPerf.gsf = float32(groundspeed)

		// ground track "True" (field 8)
		trueCourse := float32(0)
		tc, err := strconv.ParseFloat(x[8], 32)
		if err != nil && groundspeed > 3 { // some receivers return null COG at low speeds. Need to ignore this condition.
			return false
		}
		if groundspeed > 3 { //TODO: use average groundspeed over last n seconds to avoid random "jumps"
			trueCourse = float32(tc)
			setTrueCourse(uint16(groundspeed), tc)
			tmpSituation.GPSTrueCourse = trueCourse
			thisGpsPerf.coursef = float32(tc)
		} else {
			thisGpsPerf.coursef = -999.9
			// Negligible movement. Don't update course, but do use the slow speed.
			//TODO: use average course over last n seconds?
		}
		updateGPSPerf = true
		thisGpsPerf.msgType = x[0]
		tmpSituation.GPSLastGroundTrackTime = stratuxClock.Time

		// We've made it this far, so that means we've processed "everything" and can now make the change to mySituation.
		mySituation = tmpSituation

		if updateGPSPerf {
			mySituation.muGPSPerformance.Lock()
			myGPSPerfStats = append(myGPSPerfStats, thisGpsPerf)
			lenGPSPerfStats := len(myGPSPerfStats)
			//	log.Printf("GPSPerf array has %n elements. Contents are: %v\n",lenGPSPerfStats,myGPSPerfStats)
			if lenGPSPerfStats > 299 { //30 seconds @ 10 Hz for UBX, 30 seconds @ 5 Hz for MTK or SIRF with 2x messages per 200 ms)
				myGPSPerfStats = myGPSPerfStats[(lenGPSPerfStats - 299):] // remove the first n entries if more than 300 in the slice
			}
			mySituation.muGPSPerformance.Unlock()
		}

		setDataLogTimeWithGPS(mySituation)
		return true

	} else if (x[0] == "GNGSA") || (x[0] == "GPGSA") { // Satellite data.
		tmpSituation := mySituation // If we decide to not use the data in this message, then don't make incomplete changes in mySituation.

		if len(x) < 18 {
			return false
		}

		// field 1: operation mode
		// M: manual forced to 2D or 3D mode
		// A: automatic switching between 2D and 3D modes

		/*
			if (x[1] != "A") && (x[1] != "M") { // invalid fix ... but x[2] is a better indicator of fix quality. Deprecating this.
				tmpSituation.GPSFixQuality = 0 // Just a note.
				return false
			}
		*/

		// field 2: solution type
		// 1 = no solution; 2 = 2D fix, 3 = 3D fix. WAAS status is parsed from GGA message, so no need to get here
		if (x[2] == "") || (x[2] == "1") { // missing or no solution
			tmpSituation.GPSFixQuality = 0 // Just a note.
			return false
		}

		// fields 3-14: satellites in solution
		var svStr string
		var svType uint8

		// START OF PROTECTED BLOCK
		mySituation.muSatellite.Lock()

		for _, svtxt := range x[3:15] {
			sv, err := strconv.Atoi(svtxt)
			if err == nil {
				if sv <= 32 {
					svType = SAT_TYPE_GPS
					svStr = fmt.Sprintf("G%d", sv)		// GPS 1-32
				} else if sv <= 64 {
					svType = SAT_TYPE_SBAS
					svStr = fmt.Sprintf("S%d", sv+87)	// SBAS 33-64, 33 = SBAS PRN 120
				} else if sv <= 96 {
					svType = SAT_TYPE_GLONASS
					svStr = fmt.Sprintf("R%d", sv-64)	// GLONASS 65-96
				} else if sv <= 158 {
					svType = SAT_TYPE_SBAS
					svStr = fmt.Sprintf("S%d", sv-151)	// SBAS 152-158
				} else if sv <= 202 {
					svType = SAT_TYPE_QZSS
					svStr = fmt.Sprintf("Q%d", sv-192)	// QZSS 193-202
				} else if sv <= 336 {
					svType = SAT_TYPE_GALILEO
					svStr = fmt.Sprintf("E%d", sv-300)	// GALILEO 301-336
				} else if sv <= 437 {
					svType = SAT_TYPE_BEIDOU
					svStr = fmt.Sprintf("B%d", sv-400)	// BEIDOU 401-437
				} else {
					svType = SAT_TYPE_UNKNOWN
					svStr = fmt.Sprintf("U%d", sv)
				}

				var thisSatellite SatelliteInfo

				// Retrieve previous information on this satellite code.
				if val, ok := Satellites[svStr]; ok { // if we've already seen this satellite identifier, copy it in to do updates
					thisSatellite = val
					//log.Printf("Satellite %s already seen. Retrieving from 'Satellites'.\n", svStr)
				} else { // this satellite isn't in the Satellites data structure, so create it
					thisSatellite.SatelliteID = svStr
					thisSatellite.SatelliteNMEA = uint8(sv)
					thisSatellite.Type = uint8(svType)
					//log.Printf("Creating new satellite %s from GSA message\n", svStr) // DEBUG
				}
				thisSatellite.InSolution = true
				thisSatellite.TimeLastSolution = stratuxClock.Time
				thisSatellite.TimeLastSeen = stratuxClock.Time    // implied, since this satellite is used in the position solution
				thisSatellite.TimeLastTracked = stratuxClock.Time // implied, since this satellite is used in the position solution

				Satellites[thisSatellite.SatelliteID] = thisSatellite // Update constellation with this satellite
			}
		}
		updateConstellation()
		tmpSituation.GPSSatellites = mySituation.GPSSatellites
		tmpSituation.GPSSatellitesTracked = mySituation.GPSSatellitesTracked
		tmpSituation.GPSSatellitesSeen = mySituation.GPSSatellitesSeen
		mySituation.muSatellite.Unlock()
		// END OF PROTECTED BLOCK

		// field 16: HDOP
		// Accuracy estimate
		hdop, err1 := strconv.ParseFloat(x[16], 32)
		if err1 != nil {
			return false
		}
		if tmpSituation.GPSFixQuality == 2 { // Rough 95% confidence estimate for SBAS solution
			if globalStatus.GPS_detected_type == GPS_TYPE_UBX9 {			
				tmpSituation.GPSHorizontalAccuracy = float32(hdop * 3.0) 	// ublox 9
			} else {
				tmpSituation.GPSHorizontalAccuracy = float32(hdop * 4.0)	// ublox 6/7/8
			}
		} else { // Rough 95% confidence estimate non-SBAS solution
			if globalStatus.GPS_detected_type == GPS_TYPE_UBX9 {
				tmpSituation.GPSHorizontalAccuracy = float32(hdop * 4.0) 	// ublox 9
			} else {
				tmpSituation.GPSHorizontalAccuracy = float32(hdop * 5.0)	// ublox 6/7/8
			}
		}

		// NACp estimate.
		tmpSituation.GPSNACp = calculateNACp(tmpSituation.GPSHorizontalAccuracy)

		// field 17: VDOP
		// accuracy estimate
		vdop, err1 := strconv.ParseFloat(x[17], 32)
		if err1 != nil {
			return false
		}
		tmpSituation.GPSVerticalAccuracy = float32(vdop * 5) // rough estimate for 95% confidence

		// We've made it this far, so that means we've processed "everything" and can now make the change to mySituation.
		mySituation = tmpSituation
		return true

	}

	if (x[0] == "GPGSV") || (x[0] == "GLGSV") || (x[0] == "GAGSV") || (x[0] == "GBGSV") { // GPS + SBAS or GLONASS or Galileo or Beidou satellites in view message.
		if len(x) < 4 {
			return false
		}

		// field 1 = number of GSV messages of this type
		msgNum, err := strconv.Atoi(x[2])
		if err != nil {
			return false
		}

		// field 2 = index of this GSV message

		msgIndex, err := strconv.Atoi(x[2])
		if err != nil {
			return false
		}

		// field 3 = number of GPS satellites tracked
		/* Is this redundant if parsing from full constellation?
		satTracked, err := strconv.Atoi(x[3])
		if err != nil {
			return false
		}
		*/

		//mySituation.GPSSatellitesTracked = uint16(satTracked) // Replaced with parsing of 'Satellites' data structure

		// field 4-7 = repeating block with satellite id, elevation, azimuth, and signal strengh (Cno)

		lenGSV := len(x)
		satsThisMsg := (lenGSV - 4) / 4

		if globalSettings.DEBUG {
			log.Printf("%s message [%d of %d] is %v fields long and describes %v satellites\n", x[0], msgIndex, msgNum, lenGSV, satsThisMsg)
		}

		var sv, elev, az, cno int
		var svType uint8
		var svStr string

		for i := 0; i < satsThisMsg; i++ {

			sv, err = strconv.Atoi(x[4+4*i]) // sv number
			if err != nil {
				return false
			}

			if sv <= 32 {
				svType = SAT_TYPE_GPS
				svStr = fmt.Sprintf("G%d", sv)		// GPS 1-32
			} else if sv <= 64 {
				svType = SAT_TYPE_SBAS
				svStr = fmt.Sprintf("S%d", sv+87)	// SBAS 33-64, 33 = SBAS PRN 120
			} else if sv <= 96 {
				svType = SAT_TYPE_GLONASS
				svStr = fmt.Sprintf("R%d", sv-64)	// GLONASS 65-96
			} else if sv <= 158 {
				svType = SAT_TYPE_SBAS
				svStr = fmt.Sprintf("S%d", sv-151)	// SBAS 152-158
			} else if sv <= 202 {
				svType = SAT_TYPE_QZSS
				svStr = fmt.Sprintf("Q%d", sv-192)	// QZSS 193-202
			} else if sv <= 336 {
				svType = SAT_TYPE_GALILEO
				svStr = fmt.Sprintf("E%d", sv-300)	// GALILEO 301-336
			} else if sv <= 437 {
				svType = SAT_TYPE_BEIDOU
				svStr = fmt.Sprintf("B%d", sv-400)	// BEIDOU 401-437
			} else {
				svType = SAT_TYPE_UNKNOWN
				svStr = fmt.Sprintf("U%d", sv)
			}

			var thisSatellite SatelliteInfo

			// START OF PROTECTED BLOCK
			mySituation.muSatellite.Lock()

			// Retrieve previous information on this satellite code.
			if val, ok := Satellites[svStr]; ok { // if we've already seen this satellite identifier, copy it in to do updates
				thisSatellite = val
				//log.Printf("Satellite %s already seen. Retrieving from 'Satellites'.\n", svStr) // DEBUG
			} else { // this satellite isn't in the Satellites data structure, so create it new
				thisSatellite.SatelliteID = svStr
				thisSatellite.SatelliteNMEA = uint8(sv)
				thisSatellite.Type = uint8(svType)
				//log.Printf("Creating new satellite %s\n", svStr) // DEBUG
			}
			thisSatellite.TimeLastTracked = stratuxClock.Time

			elev, err = strconv.Atoi(x[5+4*i]) // elevation
			if err != nil {                    // some firmwares leave this blank if there's no position fix. Represent as -999.
				elev = -999
			}
			thisSatellite.Elevation = int16(elev)

			az, err = strconv.Atoi(x[6+4*i]) // azimuth
			if err != nil {                  // UBX allows tracking up to 5(?) degrees below horizon. Some firmwares leave this blank if no position fix. Represent invalid as -999.
				az = -999
			}
			thisSatellite.Azimuth = int16(az)

			cno, err = strconv.Atoi(x[7+4*i]) // signal
			if err != nil {                   // will be blank if satellite isn't being received. Represent as -99.
				cno = -99
				thisSatellite.InSolution = false // resets the "InSolution" status if the satellite disappears out of solution due to no signal. FIXME
				//log.Printf("Satellite %s is no longer in solution due to cno parse error - GSV\n", svStr) // DEBUG
			} else if cno > 0 {
				thisSatellite.TimeLastSeen = stratuxClock.Time // Is this needed?
			}
			if cno > 127 { // make sure strong signals don't overflow. Normal range is 0-99 so it shouldn't, but take no chances.
				cno = 127
			}
			thisSatellite.Signal = int8(cno)

			// hack workaround for GSA 12-sv limitation... if this is a SBAS satellite, we have a SBAS solution, and signal is greater than some arbitrary threshold, set InSolution
			// drawback is this will show all tracked SBAS satellites as being in solution.
			if thisSatellite.Type == SAT_TYPE_SBAS {
				if mySituation.GPSFixQuality == 2 {
					if thisSatellite.Signal > 16 {
						thisSatellite.InSolution = true
						thisSatellite.TimeLastSolution = stratuxClock.Time
					}
				} else { // quality == 0 or 1
					thisSatellite.InSolution = false
					//log.Printf("WAAS satellite %s is marked as out of solution GSV\n", svStr) // DEBUG
				}
			}

			if globalSettings.DEBUG {
				inSolnStr := " "
				if thisSatellite.InSolution {
					inSolnStr = "+"
				}
				log.Printf("GSV: Satellite %s%s at index %d. Type = %d, NMEA-ID = %d, Elev = %d, Azimuth = %d, Cno = %d\n", inSolnStr, svStr, i, svType, sv, elev, az, cno) // remove later?
			}

			Satellites[thisSatellite.SatelliteID] = thisSatellite // Update constellation with this satellite
			updateConstellation()
			mySituation.muSatellite.Unlock()
			// END OF PROTECTED BLOCK
		}

		return true
	}

	// OGN Tracker pressure data:
	// $POGNB,22.0,+29.1,100972.3,3.8,+29.4,+87.2,-0.04,+32.6,*6B
	if x[0] == "POGNB" {
		if len(x) < 5 {
			return false
		}
		var vspeed float64

		pressureAlt, err := strconv.ParseFloat(x[5], 32)
		if err != nil {
			return false
		}
		
		vspeed, err = strconv.ParseFloat(x[7], 32)
		if err != nil {
			return false
		}

		if !isTempPressValid() || mySituation.BaroSourceType != BARO_TYPE_BMP280 {
			mySituation.muBaro.Lock()
			mySituation.BaroPressureAltitude = float32(pressureAlt * 3.28084) // meters to feet
			mySituation.BaroVerticalSpeed = float32(vspeed * 196.85) // m/s in ft/min
			mySituation.BaroLastMeasurementTime = stratuxClock.Time
			mySituation.BaroSourceType = BARO_TYPE_OGNTRACKER
			mySituation.muBaro.Unlock()
		}
		return true
	}

	// Only sent by OGN tracker. We use this to detect that OGN tracker is connected and configure it as needed
	if x[0] == "POGNR" {
		if !ognTrackerConfigured {
			ognTrackerConfigured = true
			go func() {
				time.Sleep(10 * time.Second)
				configureOgnTracker()
			}()
		}

		return true
	}

	if x[0] == "POGNS" {
		// Tracker notified us of restart (crashed?) -> ensure we configure it again
		if len(x) == 2 && x[1] == "SysStart" {
			ognTrackerConfigured = false
			return true
		}
		// OGN tracker sent us its configuration
		log.Printf("Received OGN Tracker configuration: " + strings.Join(x, ","))
		for i := 1; i < len(x); i++ {
			kv := strings.SplitN(x[i], "=", 2);
			if len(kv) < 2 {
				continue
			}

			if kv[0] == "Address" {
				addr, _ :=  strconv.ParseUint(kv[1], 0, 32)
				globalSettings.OGNAddr = strings.ToUpper(fmt.Sprintf("%x", addr))
			} else if kv[0] == "AddrType" {
				addrtype, _ :=  strconv.ParseInt(kv[1], 0, 8)
				globalSettings.OGNAddrType = int(addrtype)
			} else if kv[0] == "AcftType" {
				acfttype, _ :=  strconv.ParseInt(kv[1], 0, 8)
				globalSettings.OGNAcftType = int(acfttype)
			} else if kv[0] == "Pilot" {
				globalSettings.OGNPilot = kv[1]
			}
		}
	}

	// Only evaluate PGRMZ for SoftRF/Flarm, where we know that it is standard barometric pressure.
	// might want to add more types if applicable.
	// $PGRMZ,1089,f,3*2B
	if x[0] == "PGRMZ" && ((globalStatus.GPS_detected_type & 0x0f) ==  GPS_TYPE_SERIAL || (globalStatus.GPS_detected_type & 0x0f) == GPS_TYPE_SOFTRF_DONGLE) {
		if len(x) < 3 {
			return false
		}
		// Assume pressure altitude in PGRMZ if we don't have any other baro (SoftRF style)
		pressureAlt, err := strconv.ParseFloat(x[1], 32)
		if err != nil {
			return false
		}
		unit := x[2]
		if unit == "m" {
			pressureAlt *= 3.28084
		}
		// Prefer internal sensor and OGN tracker over this...
		if !isTempPressValid() || (mySituation.BaroSourceType != BARO_TYPE_BMP280 && mySituation.BaroSourceType != BARO_TYPE_OGNTRACKER) {
			mySituation.muBaro.Lock()
			mySituation.BaroPressureAltitude = float32(pressureAlt) // meters to feet
			mySituation.BaroLastMeasurementTime = stratuxClock.Time
			mySituation.BaroSourceType = BARO_TYPE_NMEA
			mySituation.muBaro.Unlock()
			return true
		}
	}

	// Flarm NMEA traffic data
	if x[0] == "PFLAU" || x[0] == "PFLAA" {
		parseFlarmNmeaMessage(x)
		return true
	}

	// If we've gotten this far, the message isn't one that we can use.
	return false
}

func configureOgnTrackerFromSettings() {
	if serialPort == nil {
		return
	}

	cfg := fmt.Sprintf("$POGNS,Address=0x%s,AddrType=%d,AcftType=%d,Pilot=%s\r\n", globalSettings.OGNAddr, globalSettings.OGNAddrType, globalSettings.OGNAcftType, globalSettings.OGNPilot)
	log.Printf("Configuring OGN Tracker: " + cfg)

	serialPort.Write([]byte(cfg))
	serialPort.Write([]byte("$POGNS\r\n")) // re-read settings from tracker
	serialPort.Flush()
}


// Maps 1000ft bands to gnssBaroAltDiffs of known traffic.
// This will then be used to estimate our own baro altitude from GNSS if we don't have a pressure sensor connected...
// Data will receive exponential smoothing so outliers hopefully don't have too much effect
var gnssBaroAltDiffs = make(map [int]int)
func baroAltGuesser() {
	ticker := time.NewTicker(1 * time.Second)
	for {
		<-ticker.C

		avgDiff := 0
		for _, v := range gnssBaroAltDiffs {
			avgDiff += v
		}
		if len(gnssBaroAltDiffs) > 0 {
			avgDiff /= len(gnssBaroAltDiffs)
		}

		trafficMutex.Lock()
		for _, ti := range traffic {
			if ti.ReceivedMsgs < 30 || ti.SignalLevel < -28 {
				continue // Make sure it is actually a confirmed target, so we don't accidentally use invalid values from invalid data
			}
			if stratuxClock.Since(ti.Last_GnssDiff) > 1 * time.Second || ti.Alt <= 1 || stratuxClock.Since(ti.Last_alt) > 1 * time.Second {
				continue // already considered this value or we don't have a value - skip
			}
			if len(gnssBaroAltDiffs) >= 5 && math.Abs(float64(ti.GnssDiffFromBaroAlt - int32(avgDiff))) > 1000 {
				// For a simple outlier detection, disregard traffic who's gnssBaroAltDiff is more than 1000ft from the average
				continue
			}
			bucket := int(ti.Alt / 1000)
			if bucket <= 0 {
				continue // sometimes some random altitude reports - usually close to 0ft but GNSS diff from around 40000.. try to filter those
			}
			if val, ok := gnssBaroAltDiffs[bucket]; ok {
				// weighted average - don't tune too quickly... smooth over one minute (for one aircraft, half a minute for two, etc).
				gnssBaroAltDiffs[bucket] = (val * 59 + int(ti.GnssDiffFromBaroAlt) * 1) / 60
			} else {
				gnssBaroAltDiffs[bucket] = int(ti.GnssDiffFromBaroAlt)
			}
		}
		trafficMutex.Unlock()

		if len(gnssBaroAltDiffs) < 5 {
			continue // not enough data
		}
		if isGPSValid() && (!isTempPressValid() || mySituation.BaroSourceType == BARO_TYPE_NONE || mySituation.BaroSourceType == BARO_TYPE_ADSBESTIMATE) {
			// We have no real baro source.. try to estimate baro altitude with the help of closeby ADS-B aircraft that define BaroGnssDiff...

			myAlt := mySituation.GPSAltitudeMSL
			if isTempPressValid() {
				myAlt = mySituation.BaroPressureAltitude // we have something better than GPS from a previous run or something
			}
			alts := make([]float64, 0, len(gnssBaroAltDiffs))
			diffs := make([]float64, 0, len(gnssBaroAltDiffs))
			weights := make([]float64, 0, len(gnssBaroAltDiffs)) // Weigh close altitudes higher than far altitudes for linreg
			for k, v := range gnssBaroAltDiffs {
				bucketAlt := float64(k * 1000 + 500)
				alts = append(alts, bucketAlt) // Compute back from bucket to "real" altitude (+500 to be in the center of the bucket)
				diffs = append(diffs, float64(v))
				// Weight: 1 / altitudeDifference / 1000
				weight:= math.Abs(float64(myAlt) - bucketAlt)
				if weight == 0 {
					weight = 1
				} else {
					weight = math.Min(1 / (weight / 1000), 1)
				}
				weights = append(weights, weight)
			}
			if len(gnssBaroAltDiffs) >= 2 {
				slope, intercept, valid := linRegWeighted(alts, diffs, weights)
				if valid {
					gnssBaroDiff := float64(myAlt) * slope + intercept
					mySituation.muBaro.Lock()
					mySituation.BaroLastMeasurementTime = stratuxClock.Time
					mySituation.BaroPressureAltitude = mySituation.GPSHeightAboveEllipsoid - float32(gnssBaroDiff)
					mySituation.BaroSourceType = BARO_TYPE_ADSBESTIMATE
					//fmt.Printf("%f * x + %f\n", slope, intercept)
					mySituation.muBaro.Unlock()
				}
			}
		}

	}
}

func gpsSerialReader() {
	defer serialPort.Close()
	readyToInitGPS = false //TODO: replace with channel control to terminate goroutine when complete

	i := 0 //debug monitor
	scanner := bufio.NewScanner(serialPort)
	for scanner.Scan() && globalStatus.GPS_connected && globalSettings.GPS_Enabled {
		i++
		if globalSettings.DEBUG && i%100 == 0 {
			log.Printf("gpsSerialReader() scanner loop iteration i=%d\n", i) // debug monitor
		}

		s := scanner.Text()
		startIdx := strings.Index(s, "$")
		if startIdx < 0 {
			continue
		}
		s = s[startIdx:]

		if !processNMEALine(s) {
			if globalSettings.DEBUG {
				fmt.Printf("processNMEALine() exited early -- %s\n", s)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("reading standard input: %s\n", err.Error())
	}

	if globalSettings.DEBUG {
		log.Printf("Exiting gpsSerialReader() after i=%d loops\n", i) // debug monitor
	}
	globalStatus.GPS_connected = false
	readyToInitGPS = true //TODO: replace with channel control to terminate goroutine when complete
	return
}

func makeAHRSSimReport() {
	sendXPlane(createXPlaneAttitudeMsg(float32(mySituation.AHRSGyroHeading), float32(mySituation.AHRSPitch), float32(mySituation.AHRSRoll)), false)
}

/*

	ForeFlight "AHRS Message".

	Sends AHRS information to ForeFlight.

*/

func makeFFAHRSMessage() {
	msg := make([]byte, 12)
	msg[0] = 0x65 // Message type "ForeFlight".
	msg[1] = 0x01 // AHRS message identifier.

	// Values if invalid
	pitch := int16(0x7FFF)
	roll := int16(0x7FFF)
	hdg := uint16(0xFFFF)
	ias := uint16(0xFFFF)
	tas := uint16(0xFFFF)

	if isAHRSValid() {
		if !isAHRSInvalidValue(mySituation.AHRSPitch) {
			pitch = roundToInt16(mySituation.AHRSPitch * 10)
		}
		if !isAHRSInvalidValue(mySituation.AHRSRoll) {
			roll = roundToInt16(mySituation.AHRSRoll * 10)
		}
	}

	// Roll.
	msg[2] = byte((roll >> 8) & 0xFF)
	msg[3] = byte(roll & 0xFF)

	// Pitch.
	msg[4] = byte((pitch >> 8) & 0xFF)
	msg[5] = byte(pitch & 0xFF)

	// Heading.
	msg[6] = byte((hdg >> 8) & 0xFF)
	msg[7] = byte(hdg & 0xFF)

	// Indicated Airspeed.
	msg[8] = byte((ias >> 8) & 0xFF)
	msg[9] = byte(ias & 0xFF)

	// True Airspeed.
	msg[10] = byte((tas >> 8) & 0xFF)
	msg[11] = byte(tas & 0xFF)

	sendMsg(prepareMessage(msg), NETWORK_AHRS_GDL90, false)
}

/*
	ffAttitudeSender()
	 Send AHRS message in FF format every 200ms.
*/

func ffAttitudeSender() {
	ticker := time.NewTicker(200 * time.Millisecond)
	for {
		<-ticker.C
		makeFFAHRSMessage()
	}
}

func makeAHRSGDL90Report() {
	msg := make([]byte, 24)
	msg[0] = 0x4c
	msg[1] = 0x45
	msg[2] = 0x01
	msg[3] = 0x01

	// Values if invalid
	pitch := int16(0x7FFF)
	roll := int16(0x7FFF)
	hdg := int16(0x7FFF)
	slip_skid := int16(0x7FFF)
	yaw_rate := int16(0x7FFF)
	g := int16(0x7FFF)
	airspeed := int16(0x7FFF) // Can add this once we can read airspeed
	palt := uint16(0xFFFF)
	vs := int16(0x7FFF)
	if isAHRSValid() {
		if !isAHRSInvalidValue(mySituation.AHRSPitch) {
			pitch = roundToInt16(mySituation.AHRSPitch * 10)
		}
		if !isAHRSInvalidValue(mySituation.AHRSRoll) {
			roll = roundToInt16(mySituation.AHRSRoll * 10)
		}
		if !isAHRSInvalidValue(mySituation.AHRSGyroHeading) {
			hdg = roundToInt16(mySituation.AHRSGyroHeading * 10)
		}
		if !isAHRSInvalidValue(mySituation.AHRSSlipSkid) {
			slip_skid = roundToInt16(-mySituation.AHRSSlipSkid * 10)
		}
		if !isAHRSInvalidValue(mySituation.AHRSTurnRate) {
			yaw_rate = roundToInt16(mySituation.AHRSTurnRate * 10)
		}
		if !isAHRSInvalidValue(mySituation.AHRSGLoad) {
			g = roundToInt16(mySituation.AHRSGLoad * 10)
		}
	}
	if isTempPressValid() {
		palt = uint16(mySituation.BaroPressureAltitude + 5000.5)
		vs = roundToInt16(float64(mySituation.BaroVerticalSpeed))
	}

	// Roll.
	msg[4] = byte((roll >> 8) & 0xFF)
	msg[5] = byte(roll & 0xFF)

	// Pitch.
	msg[6] = byte((pitch >> 8) & 0xFF)
	msg[7] = byte(pitch & 0xFF)

	// Heading.
	msg[8] = byte((hdg >> 8) & 0xFF)
	msg[9] = byte(hdg & 0xFF)

	// Slip/skid.
	msg[10] = byte((slip_skid >> 8) & 0xFF)
	msg[11] = byte(slip_skid & 0xFF)

	// Yaw rate.
	msg[12] = byte((yaw_rate >> 8) & 0xFF)
	msg[13] = byte(yaw_rate & 0xFF)

	// "G".
	msg[14] = byte((g >> 8) & 0xFF)
	msg[15] = byte(g & 0xFF)

	// Indicated Airspeed
	msg[16] = byte((airspeed >> 8) & 0xFF)
	msg[17] = byte(airspeed & 0xFF)

	// Pressure Altitude
	msg[18] = byte((palt >> 8) & 0xFF)
	msg[19] = byte(palt & 0xFF)

	// Vertical Speed
		msg[20] = byte((vs >> 8) & 0xFF)
	msg[21] = byte(vs & 0xFF)

	// Reserved
	msg[22] = 0x7F
	msg[23] = 0xFF

	sendMsg(prepareMessage(msg), NETWORK_AHRS_GDL90, false)
}

func gpsAttitudeSender() {
	timer := time.NewTicker(100 * time.Millisecond) // ~10Hz update.
	for {
		<-timer.C
		if !(globalStatus.GPS_connected || globalStatus.IMUConnected) {
 			myGPSPerfStats = make([]gpsPerfStats, 0) // reinitialize statistics on disconnect / reconnect
 		} else {
			mySituation.muGPSPerformance.Lock()
			calculateNavRate()
			mySituation.muGPSPerformance.Unlock()
 		}

		for !(globalSettings.IMU_Sensor_Enabled && globalStatus.IMUConnected) && (globalSettings.GPS_Enabled && globalStatus.GPS_connected) {
			<-timer.C

			if !isGPSValid() || !calcGPSAttitude() {
				if globalSettings.DEBUG {
					log.Printf("Couldn't calculate GPS-based attitude statistics\n")
				}
			} else {
				mySituation.muGPSPerformance.Lock()
				index := len(myGPSPerfStats) - 1
				if index > 1 {
					mySituation.AHRSPitch = myGPSPerfStats[index].gpsPitch
					mySituation.AHRSRoll = myGPSPerfStats[index].gpsRoll
					mySituation.AHRSGyroHeading = float64(mySituation.GPSTrueCourse)
					mySituation.AHRSLastAttitudeTime = stratuxClock.Time

					makeAHRSGDL90Report()
					makeAHRSSimReport()
				}
				mySituation.muGPSPerformance.Unlock()
			}
		}
	}
}

/*
	updateConstellation(): Periodic cleanup and statistics calculation for 'Satellites'
		data structure. Calling functions must protect this in a mySituation.muSatellite.

*/

func updateConstellation() {
	var sats, tracked, seen uint8
	for svStr, thisSatellite := range Satellites {
		if stratuxClock.Since(thisSatellite.TimeLastTracked) > 10*time.Second { // remove stale satellites if they haven't been tracked for 10 seconds
			delete(Satellites, svStr)
		} else { // satellite almanac data is "fresh" even if it isn't being received.
			tracked++
			if thisSatellite.Signal > 0 {
				seen++
			}
			if stratuxClock.Since(thisSatellite.TimeLastSolution) > 5*time.Second {
				thisSatellite.InSolution = false
				Satellites[svStr] = thisSatellite
			}
			if thisSatellite.InSolution { // TESTING: Determine "In solution" from structure (fix for multi-GNSS overflow)
				sats++
			}
			// do any other calculations needed for this satellite
		}
	}

	mySituation.GPSSatellites = uint16(sats)
	mySituation.GPSSatellitesTracked = uint16(tracked)
	mySituation.GPSSatellitesSeen = uint16(seen)
}

func isGPSConnected() bool {
	return stratuxClock.Since(mySituation.GPSLastValidNMEAMessageTime) < 5*time.Second
}

/*
isGPSValid returns true only if a valid position fix has been seen in the last 3 seconds,
and if the GPS subsystem has recently detected a GPS device.

If false, 'GPSFixQuality` is set to 0 ("No fix"), as is the number of satellites in solution.
*/

func isGPSValid() bool {
	isValid := false
	if (stratuxClock.Since(mySituation.GPSLastFixLocalTime) < 3*time.Second) && globalStatus.GPS_connected && mySituation.GPSFixQuality > 0 {
		isValid = true
	} else {
		mySituation.GPSFixQuality = 0
		mySituation.GPSSatellites = 0
		mySituation.GPSHorizontalAccuracy = 999999
		mySituation.GPSVerticalAccuracy = 999999
		mySituation.GPSNACp = 0
	}
	return isValid
}

/*
isGPSGroundTrackValid returns true only if a valid ground track was obtained in the last 3 seconds,
and if NACp >= 9.
*/

func isGPSGroundTrackValid() bool {
	return isGPSValid() &&
		(mySituation.GPSHorizontalAccuracy < 30)
}

func isGPSClockValid() bool {
	return stratuxClock.Since(mySituation.GPSLastGPSTimeStratuxTime).Seconds() < 15
}

func isAHRSValid() bool {
	// If attitude information gets to be over 1 second old, declare invalid.
	// If no GPS then we won't use or send attitude information.
	return (globalSettings.DeveloperMode || isGPSValid()) && stratuxClock.Since(mySituation.AHRSLastAttitudeTime).Seconds() < 1
}

func isTempPressValid() bool {
	return stratuxClock.Since(mySituation.BaroLastMeasurementTime).Seconds() < 15
}

func pollGPS() {
	readyToInitGPS = true //TODO: Implement more robust method (channel control) to kill zombie serial readers
	timer := time.NewTicker(4 * time.Second)
	go gpsAttitudeSender()
	go ffAttitudeSender()
	for {
		<-timer.C
		// GPS enabled, was not connected previously?
		if globalSettings.GPS_Enabled && !globalStatus.GPS_connected && readyToInitGPS { //TODO: Implement more robust method (channel control) to kill zombie serial readers
			globalStatus.GPS_connected = initGPSSerial()
			if globalStatus.GPS_connected && (globalStatus.GPS_detected_type & 0x0f) != GPS_TYPE_NETWORK {
				go gpsSerialReader()
			}
		}
	}
}

func initGPS() {
	Satellites = make(map[string]SatelliteInfo)

	go pollGPS()
}
