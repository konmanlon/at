package at

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xlab/at/calls"
	"github.com/xlab/at/pdu"
	"github.com/xlab/at/sms"
	"github.com/xlab/at/util"
)

// DeviceProfile hides the device-specific implementation
// and provides a set of methods that can be used on a device.
// Init should be called first.
type DeviceProfile interface {
	Init(*Device) error
	CMGS(length int, octets []byte) (byte, error)
	CUSD(reporting Opt, octets []byte, enc Encoding) (err error)
	CMGR(index uint16) (octets []byte, err error)
	CMGD(index uint16, option Opt) (err error)
	CMGL(flag Opt) (octets []MessageSlot, err error)
	CMGF(text bool) (err error)
	CLIP(text bool) (err error)
	CHUP() (err error)
	CNMI(mode, mt, bm, ds, bfr int) (err error)
	CPMS(mem1 StringOpt, mem2 StringOpt, mem3 StringOpt) (err error)
	BOOT(token uint64) (err error)
	SYSCFG(roaming, cellular bool) (err error)
	SYSINFO() (info *SystemInfoReport, err error)
	COPS(auto bool, text bool) (err error)
	OperatorName() (str string, err error)
	ModelName() (str string, err error)
	IMEI() (str string, err error)
}

// DeviceE173 returns an instance of DeviceProfile implementation for Huawei E173,
// it's also the default one.
func DeviceE173() DeviceProfile {
	return &DefaultProfile{}
}

// DefaultProfile is a reference implementation that could be embedded
// in any other custom implementation of the DeviceProfile interface.
type DefaultProfile struct {
	dev *Device
	DeviceProfile
}

// Init invokes a set of methods that will make the initial setup of the modem.
func (p *DefaultProfile) Init(d *Device) (err error) {
	p.dev = d
	p.dev.Send(NoopCmd) // kinda flush
	if err = p.COPS(true, true); err != nil {
		return fmt.Errorf("at init: unable to adjust the format of operator's name: %w", err)
	}
	var info *SystemInfoReport
	if info, err = p.SYSINFO(); err != nil {
		return fmt.Errorf("at init: unable to read system info: %w", err)
	}
	p.dev.State = &DeviceState{
		ServiceState:  info.ServiceState,
		ServiceDomain: info.ServiceDomain,
		RoamingState:  info.RoamingState,
		SystemMode:    info.SystemMode,
		SystemSubmode: info.SystemSubmode,
		SimState:      info.SimState,
	}
	if p.dev.State.OperatorName, err = p.OperatorName(); err != nil {
		return fmt.Errorf("at init: unable to read operator's name: %w", err)
	}
	if p.dev.State.ModelName, err = p.ModelName(); err != nil {
		return fmt.Errorf("at init: unable to read modem's model name: %w", err)
	}
	if p.dev.State.IMEI, err = p.IMEI(); err != nil {
		return fmt.Errorf("at init: unable to read modem's IMEI code: %w", err)
	}
	if err = p.CMGF(false); err != nil {
		return fmt.Errorf("at init: unable to switch message format to PDU: %w", err)
	}
	if err = p.CPMS(MemoryTypes.NvRAM, MemoryTypes.NvRAM, MemoryTypes.NvRAM); err != nil {
		return fmt.Errorf("at init: unable to set messages storage: %w", err)
	}
	if err = p.CNMI(1, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("at init: unable to turn on message notifications: %w", err)
	}
	if err = p.CLIP(true); err != nil {
		return fmt.Errorf("at init: unable to turn on calling party ID notifications: %w", err)
	}

	return p.FetchInbox()
}

func (p *DefaultProfile) FetchInbox() error {
	slots, err := p.CMGL(MessageFlags.Any)
	if err != nil {
		return fmt.Errorf("unable to check message inbox: %w", err)
	}

	for i := range slots {
		var msg sms.Message
		if _, err := msg.ReadFrom(slots[i].Payload); err != nil {
			return fmt.Errorf("error while parsing message inbox: %w", err)
		}
		if err := p.CMGD(slots[i].Index, DeleteOptions.Index); err != nil {
			return fmt.Errorf("error while cleaning message inbox: %w", err)
		}
		p.dev.messages <- &msg
	}
	return nil
}

type signalStrengthReport uint64

func (s *signalStrengthReport) Parse(str string) error {
	u, err := parseUint8(str)
	*s = signalStrengthReport(u)
	return err
}

type modeReport struct {
	Mode    Opt
	Submode Opt
}

func (m *modeReport) Parse(str string) (err error) {
	fields := strings.Split(str, ",")
	if len(fields) < 2 {
		return ErrParseReport
	}

	mode, err := parseUint8(fields[0])
	if err != nil {
		return err
	}

	submode, err := parseUint8(fields[1])
	if err != nil {
		return err
	}

	m.Mode = SystemModes.Resolve(int(mode))
	m.Submode = SystemSubmodes.Resolve(int(submode))
	return
}

type simStateReport Opt

func (s *simStateReport) Parse(str string) (err error) {
	o, err := parseUint8(str)
	if err != nil {
		return err
	}

	*s = simStateReport(SimStates.Resolve(int(o)))
	return nil
}

type serviceStateReport Opt

func (s *serviceStateReport) Parse(str string) error {
	i, err := parseUint8(str)
	if err != nil {
		return err
	}

	*s = serviceStateReport(ServiceStates.Resolve(int(i)))
	return nil
}

type bootHandshakeReport uint64

func (b *bootHandshakeReport) Parse(str string) error {
	fields := strings.Split(str, ",")
	if len(fields) < 1 {
		return ErrParseReport
	}

	key, err := parseUint8(fields[0])
	if err != nil {
		return err
	}

	*b = bootHandshakeReport(key)
	return nil
}

// Ussd type represents the USSD query string.
type Ussd string

// Encode converts the query string into bytes according to the
// specified encoding.
func (u *Ussd) Encode(enc Encoding) ([]byte, error) {
	switch enc {
	case Encodings.Gsm7Bit:
		return pdu.Encode7Bit(u.String()), nil
	case Encodings.UCS2:
		return pdu.EncodeUcs2(u.String()), nil
	default:
		return nil, ErrUnknownEncoding
	}
}

func (u *Ussd) String() string {
	return string(*u)
}

type ussdReport struct {
	N      uint8
	Octets []byte
	Enc    Encoding
}

func (r *ussdReport) Parse(str string) (err error) {
	fields := strings.Split(str, ",")
	if len(fields) < 3 {
		return ErrParseReport
	}
	if r.N, err = parseUint8(fields[0]); err != nil {
		return
	}
	if r.Octets, err = util.Bytes(strings.Trim(fields[1], `"`)); err != nil {
		return
	}
	var e uint8
	if e, err = parseUint8(fields[2]); err != nil {
		return
	}
	r.Enc = Encoding(e)
	return
}

// CUSD sends AT+CUSD with the given parameters to the device. This will invoke an USSD request.
func (p *DefaultProfile) CUSD(reporting Opt, octets []byte, enc Encoding) (err error) {
	req := fmt.Sprintf(`AT+CUSD=%d,%02X,%d`, reporting.ID, octets, enc)
	_, err = p.dev.Send(req)
	return
}

type callerIDReport struct {
	CallerID   string
	IDType     Opt
	IDValidity Opt
}

func (c *callerIDReport) Parse(str string) (err error) {
	fields := strings.Split(str, ",")
	if len(fields) != 6 {
		return ErrParseReport
	}

	c.CallerID = strings.Trim(fields[0], "\"")

	var t uint8
	if t, err = parseUint8(fields[1]); err != nil {
		return
	}
	c.IDType = CallerIDTypes.Resolve(int(t))

	var v uint8
	if v, err = parseUint8(fields[5]); err != nil {
		return
	}
	c.IDType = CallerIDTypes.Resolve(int(v))

	return nil
}

func (c *callerIDReport) GetCallerID() *calls.CallerID {
	return &calls.CallerID{
		CallerID:   c.CallerID,
		IDType:     c.IDType.ID,
		IDValidity: c.IDValidity.ID,
	}
}

type messageReport struct {
	Memory StringOpt
	Index  uint16
}

func (m *messageReport) Parse(str string) (err error) {
	fields := strings.Split(str, ",")
	if len(fields) < 2 {
		return ErrParseReport
	}
	if m.Memory = MemoryTypes.Resolve(strings.Trim(fields[0], `"`)); m.Memory == UnknownStringOpt {
		return ErrParseReport
	}
	if m.Index, err = parseUint16(fields[1]); err != nil {
		return
	}
	return
}

// CMGR sends AT+CMGR with the given index to the device and returns the message contents.
func (p *DefaultProfile) CMGR(index uint16) (octets []byte, err error) {
	req := fmt.Sprintf(`AT+CMGR=%d`, index)
	reply, err := p.dev.Send(req)
	if err != nil {
		return
	}
	lines := strings.Split(reply, "\n")
	if len(lines) < 2 {
		return nil, ErrParseReport
	}
	octets, err = util.Bytes(lines[1])
	return
}

// CMGD sends AT+CMGD with the given index and option to the device. Option defines the mode
// in which messages will be deleted. The default mode is to delete by index.
func (p *DefaultProfile) CMGD(index uint16, option Opt) (err error) {
	req := fmt.Sprintf(`AT+CMGD=%d,%d`, index, option.ID)
	_, err = p.dev.Send(req)
	return
}

// CPMS sends AT+CPMS with the given options to the device. It allows to select
// the storage type for different kinds of messages and message notifications.
func (p *DefaultProfile) CPMS(mem1 StringOpt, mem2 StringOpt, mem3 StringOpt) (err error) {
	req := fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, mem1.ID, mem2.ID, mem3.ID)
	_, err = p.dev.Send(req)
	return
}

// CNMI sends AT+CNMI with the given parameters to the device.
// It's used to adjust the settings of the new message arrival notifications.
func (p *DefaultProfile) CNMI(mode, mt, bm, ds, bfr int) (err error) {
	req := fmt.Sprintf(`AT+CNMI=%d,%d,%d,%d,%d`, mode, mt, bm, ds, bfr)
	_, err = p.dev.Send(req)
	return
}

// CMGF sends AT+CMGF with the given value to the device. It toggles
// the mode of message handling between PDU and TEXT.
//
// Note, that the at package works only in PDU mode.
func (p *DefaultProfile) CMGF(text bool) (err error) {
	var flag int
	if text {
		flag = 1
	}
	req := fmt.Sprintf(`AT+CMGF=%d`, flag)
	_, err = p.dev.Send(req)
	return
}

// CLIP sends AT+CLIP with the given value to the device. It toggles
// the mode of periodic calling party ID notification
func (p *DefaultProfile) CLIP(text bool) (err error) {
	var flag int
	if text {
		flag = 1
	}
	req := fmt.Sprintf(`AT+CLIP=%d`, flag)
	_, err = p.dev.Send(req)
	return
}

// CHUP sends ATH+CHUP to the device. It hangs up
// an active incoming call
func (p *DefaultProfile) CHUP() (err error) {
	req := "ATH+CHUP"
	_, err = p.dev.Send(req)
	return
}

type MessageSlot struct {
	Index   uint16
	Payload []byte
}

// CMGL sends AT+CMGL with the given filtering flag to the device and then parses
// the list of received messages that match their filter. See MessageFlags for the
// list of supported filters.
func (p *DefaultProfile) CMGL(flag Opt) (result []MessageSlot, err error) {
	req := fmt.Sprintf(`AT+CMGL=%d`, flag.ID)
	reply, err := p.dev.Send(req)
	if err != nil {
		return
	}
	lines := strings.Split(reply, "\n")
	if len(lines) < 2 {
		return
	}

	for i := 0; i < len(lines); i += 2 {
		header := strings.TrimPrefix(lines[i], `+CMGL: `)
		fields := strings.Split(header, ",")
		if len(fields) < 4 {
			return nil, ErrParseReport
		}
		n, err := parseUint16(fields[0])
		if err != nil {
			return nil, ErrParseReport
		}
		var oct []byte
		if oct, err = util.Bytes(lines[i+1]); err != nil {
			return nil, ErrParseReport
		}

		result = append(result, MessageSlot{
			Index:   n,
			Payload: oct,
		})
	}
	return
}

// BOOT sends AT^BOOT with the given token to the device. This completes
// the handshaking procedure.
func (p *DefaultProfile) BOOT(token uint64) (err error) {
	req := fmt.Sprintf(`AT^BOOT=%d,0`, token)
	_, err = p.dev.Send(req)
	return
}

// CMGS sends AT+CMGS with the given parameters to the device. This is used to send SMS
// using the given PDU data. Length is a number of TPDU bytes.
// Returns the reference number of the sent message.
func (p *DefaultProfile) CMGS(length int, octets []byte) (byte, error) {
	part1 := fmt.Sprintf("AT+CMGS=%d", length)
	part2 := fmt.Sprintf("%02X", octets)
	reply, err := p.dev.sendInteractive(part1, part2, byte('>'))

	if err != nil {
		return 0, err
	}

	if !strings.HasPrefix(reply, "+CMGS: ") {
		return 0, fmt.Errorf("unable to get sequence number of reply '%s'", reply)
	}

	number, err := parseUint8(reply[7:])
	if err != nil {
		return 0, fmt.Errorf("unable to parse sequence number of reply '%s': %w", reply, err)
	}

	return byte(number), nil
}

// SYSCFG sends AT^SYSCFG with the given parameters to the device.
// The arguments of this command may vary, so the options are limited to switchng roaming and
// cellular mode on/off.
func (p *DefaultProfile) SYSCFG(roaming, cellular bool) (err error) {
	var roam int
	if roaming {
		roam = 1
	}
	var cell int
	if cellular {
		cell = 2
	} else {
		cell = 1
	}
	req := fmt.Sprintf(`AT^SYSCFG=2,2,3FFFFFFF,%d,%d`, roam, cell)
	_, err = p.dev.Send(req)
	return
}

// SystemInfoReport represents the report from the AT^SYSINFO command.
type SystemInfoReport struct {
	ServiceState  Opt
	ServiceDomain Opt
	RoamingState  Opt
	SystemMode    Opt
	SystemSubmode Opt
	SimState      Opt
}

// Parse scans the AT^SYSINFO report into a non-nil SystemInfoReport struct.
func (s *SystemInfoReport) Parse(str string) (err error) {
	fields := strings.Split(str, ",")
	if len(fields) < 7 {
		return ErrParseReport
	}

	fetch := func(str string, field *Opt, resolver func(id int) Opt) error {
		if n, err := parseUint8(str); err != nil {
			return err
		} else if opt := resolver(int(n)); opt == UnknownOpt {
			return errors.New("resolver: unknown opt")
		} else {
			*field = opt
			return nil
		}
	}

	if err = fetch(fields[0], &s.ServiceState, ServiceStates.Resolve); err != nil {
		return ErrParseReport
	}
	if err = fetch(fields[1], &s.ServiceDomain, ServiceDomains.Resolve); err != nil {
		return ErrParseReport
	}
	if err = fetch(fields[2], &s.RoamingState, RoamingStates.Resolve); err != nil {
		return ErrParseReport
	}
	if err = fetch(fields[3], &s.SystemMode, SystemModes.Resolve); err != nil {
		return ErrParseReport
	}
	if err = fetch(fields[4], &s.SimState, SimStates.Resolve); err != nil {
		return ErrParseReport
	}
	if err = fetch(fields[6], &s.SystemSubmode, SystemSubmodes.Resolve); err != nil {
		return ErrParseReport
	}
	return nil
}

// SYSINFO sends AT^SYSINFO to the device and parses the output.
func (p *DefaultProfile) SYSINFO() (info *SystemInfoReport, err error) {
	reply, err := p.dev.Send(`AT^SYSINFO`)
	if err != nil {
		return nil, err
	}
	info = new(SystemInfoReport)
	err = info.Parse(strings.TrimPrefix(reply, `^SYSINFO:`))
	return
}

// COPS sends AT+COPS to the device with parameters that define autosearch and
// the operator's name representation. The default representation is numerical.
func (p *DefaultProfile) COPS(auto bool, text bool) (err error) {
	var a, t int
	if !auto {
		a = 1
	}
	if !text {
		t = 2
	}
	req := fmt.Sprintf(`AT+COPS=%d,%d`, a, t)
	_, err = p.dev.Send(req)
	return
}

// OperatorName sends AT+COPS? to the device and gets the operator's name.
func (p *DefaultProfile) OperatorName() (str string, err error) {
	result, err := p.dev.Send(`AT+COPS?`)
	fields := strings.Split(strings.TrimPrefix(result, `+COPS: `), ",")
	if len(fields) < 4 {
		err = ErrParseReport
		return
	}
	str = strings.TrimLeft(strings.TrimRight(fields[2], `"`), `"`)
	return
}

// ModelName sends AT+GMM to the device and gets the modem's model name.
func (p *DefaultProfile) ModelName() (str string, err error) {
	str, err = p.dev.Send(`AT+GMM`)
	return
}

// IMEI sends AT+GSN to the device and gets the modem's IMEI code.
func (p *DefaultProfile) IMEI() (str string, err error) {
	str, err = p.dev.Send(`AT+GSN`)
	return
}

// Air72x
type Air72xProfile struct {
	dev *Device
	DeviceProfile
}

func (p *Air72xProfile) Init(d *Device) (err error) {
	p.dev = d
	p.dev.Send(NoopCmd) // kinda flush
	if err = p.COPS(true, true); err != nil {
		return fmt.Errorf("at init: unable to adjust the format of operator's name: %w", err)
	}
	var info *SystemInfoReport
	if info, err = p.SYSINFO(); err != nil {
		return fmt.Errorf("at init: unable to read system info: %w", err)
	}
	p.dev.State = &DeviceState{
		ServiceState:  info.ServiceState,
		ServiceDomain: info.ServiceDomain,
		RoamingState:  info.RoamingState,
		SystemMode:    info.SystemMode,
		SystemSubmode: info.SystemSubmode,
		SimState:      info.SimState,
	}
	if p.dev.State.OperatorName, err = p.OperatorName(); err != nil {
		return fmt.Errorf("at init: unable to read operator's name: %w", err)
	}
	if p.dev.State.ModelName, err = p.ModelName(); err != nil {
		return fmt.Errorf("at init: unable to read modem's model name: %w", err)
	}
	if p.dev.State.IMEI, err = p.IMEI(); err != nil {
		return fmt.Errorf("at init: unable to read modem's IMEI code: %w", err)
	}
	if err = p.CMGF(false); err != nil {
		return fmt.Errorf("at init: unable to switch message format to PDU: %w", err)
	}
	if err = p.CPMS(MemoryTypes.Sim, MemoryTypes.Sim, MemoryTypes.Sim); err != nil {
		return fmt.Errorf("at init: unable to set messages storage: %w", err)
	}
	if err = p.CNMI(2, 2, 0, 0, 0); err != nil {
		return fmt.Errorf("at init: unable to turn on message notifications: %w", err)
	}
	if err = p.CLIP(true); err != nil {
		return fmt.Errorf("at init: unable to turn on calling party ID notifications: %w", err)
	}

	return p.FetchInbox()
}

func (p *Air72xProfile) FetchInbox() error {
	slots, err := p.CMGL(MessageFlags.Any)
	if err != nil {
		return fmt.Errorf("unable to check message inbox: %w", err)
	}

	for i := range slots {
		var msg sms.Message
		if _, err := msg.ReadFrom(slots[i].Payload); err != nil {
			return fmt.Errorf("error while parsing message inbox: %w", err)
		}
		// if err := p.CMGD(slots[i].Index, DeleteOptions.Index); err != nil {
		// 	return fmt.Errorf("error while cleaning message inbox: %w", err)
		// }
		p.dev.messages <- &msg
	}
	return nil
}

// 发送短信
func (p *Air72xProfile) CMGS(length int, octets []byte) (byte, error) {
	part1 := fmt.Sprintf("AT+CMGS=%d", length)
	part2 := fmt.Sprintf("%02X", octets)
	reply, err := p.dev.sendInteractive(part1, part2, byte('>'))

	if err != nil {
		return 0, err
	}

	if !strings.HasPrefix(reply, "+CMGS: ") {
		return 0, fmt.Errorf("unable to get sequence number of reply '%s'", reply)
	}

	number, err := parseUint8(reply[7:])
	if err != nil {
		return 0, fmt.Errorf("unable to parse sequence number of reply '%s': %w", reply, err)
	}

	return byte(number), nil
}

func (p *Air72xProfile) CUSD(reporting Opt, octets []byte, enc Encoding) (err error) {
	return errors.New("this method is unavailable")
}

// 读取短信
func (p *Air72xProfile) CMGR(index uint16) (octets []byte, err error) {
	req := fmt.Sprintf(`AT+CMGR=%d`, index)
	reply, err := p.dev.Send(req)
	if err != nil {
		return
	}
	lines := strings.Split(reply, "\n")
	if len(lines) < 2 {
		return nil, ErrParseReport
	}
	octets, err = util.Bytes(lines[1])
	return
}

// 删除短信
func (p *Air72xProfile) CMGD(index uint16, option Opt) (err error) {
	req := fmt.Sprintf(`AT+CMGD=%d,%d`, index, option.ID)
	_, err = p.dev.Send(req)
	return
}

// 列举短信
func (p *Air72xProfile) CMGL(flag Opt) (result []MessageSlot, err error) {
	req := fmt.Sprintf(`AT+CMGL=%d`, flag.ID)
	reply, err := p.dev.Send(req)
	if err != nil {
		return
	}
	lines := strings.Split(reply, "\n")
	if len(lines) < 2 {
		return
	}

	for i := 0; i < len(lines); i += 2 {
		header := strings.TrimPrefix(lines[i], `+CMGL: `)
		fields := strings.Split(header, ",")
		if len(fields) < 4 {
			return nil, ErrParseReport
		}
		n, err := parseUint16(fields[0])
		if err != nil {
			return nil, ErrParseReport
		}
		var oct []byte
		if oct, err = util.Bytes(lines[i+1]); err != nil {
			return nil, ErrParseReport
		}

		result = append(result, MessageSlot{
			Index:   n,
			Payload: oct,
		})
	}
	return
}

// 设置短信格式
func (p *Air72xProfile) CMGF(text bool) (err error) {
	var flag int
	if text {
		flag = 1
	}
	req := fmt.Sprintf(`AT+CMGF=%d`, flag)
	_, err = p.dev.Send(req)
	return
}

// 设置主叫号码显示
func (p *Air72xProfile) CLIP(text bool) (err error) {
	var flag int
	if text {
		flag = 1
	}
	req := fmt.Sprintf(`AT+CLIP=%d`, flag)
	_, err = p.dev.Send(req)
	return
}

// 挂断通话
func (p *Air72xProfile) CHUP() (err error) {
	req := "ATH+CHUP"
	_, err = p.dev.Send(req)
	return
}

// 新消息指示
func (p *Air72xProfile) CNMI(mode, mt, bm, ds, bfr int) (err error) {
	req := fmt.Sprintf(`AT+CNMI=%d,%d,%d,%d,%d`, mode, mt, bm, ds, bfr)
	_, err = p.dev.Send(req)
	return
}

// 短信优先存储区域选择
func (p *Air72xProfile) CPMS(mem1 StringOpt, mem2 StringOpt, mem3 StringOpt) (err error) {
	req := fmt.Sprintf(`AT+CPMS="%s","%s","%s"`, mem1.ID, mem2.ID, mem3.ID)
	_, err = p.dev.Send(req)
	return
}

func (p *Air72xProfile) BOOT(token uint64) (err error) {
	return errors.New("this method is unavailable")
}

// 仅用于漫游模式切换
func (p *Air72xProfile) SYSCFG(roaming, cellular bool) (err error) {
	var roam int
	if roaming {
		// 0: roaming disabled
		// 1: roaming enabled
		roam = 1
	}
	req := fmt.Sprintf(`AT^SYSCONFIG=2,3,%d,%d`, roam, 2)
	_, err = p.dev.Send(req)
	return
}

func (p *Air72xProfile) SYSINFO() (info *SystemInfoReport, err error) {
	reply, err := p.dev.Send(`AT^SYSINFO`)
	if err != nil {
		return nil, err
	}
	info = new(SystemInfoReport)
	err = info.Parse(strings.TrimPrefix(reply, `^SYSINFO:`))
	return
}

// 选择运营商
func (p *Air72xProfile) COPS(auto bool, text bool) (err error) {
	var a, t int
	if auto {
		// 0: 自动注册运营商
		a = 0
	}
	if text {
		// 2: 运营商的格式为数字式字符串型
		t = 2
	}
	// AT+COPS=<mode>[,<format>[,<oper>[,<AcT>[,<Domain>]]]]
	req := fmt.Sprintf(`AT+COPS=%d,%d`, a, t)
	_, err = p.dev.Send(req)
	return
}

// 查询运营商
func (p *Air72xProfile) OperatorName() (str string, err error) {
	result, err := p.dev.Send(`AT+COPS?`)
	fields := strings.Split(strings.TrimPrefix(result, `+COPS: `), ",")
	if len(fields) < 4 {
		err = ErrParseReport
		return
	}
	str = strings.TrimLeft(strings.TrimRight(fields[2], `"`), `"`)
	return
}

func (p *Air72xProfile) ModelName() (str string, err error) {
	str, err = p.dev.Send(`AT+CGMM`)
	return
}

func (p *Air72xProfile) IMEI() (str string, err error) {
	str, err = p.dev.Send(`AT+CGSN`)
	return
}
