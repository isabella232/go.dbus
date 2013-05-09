package dbus

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"
	"strconv"
)

const protoVersion byte = 1

// Flags represents the possible flags of a DBus message.
type Flags byte

const (
	FlagNoReplyExpected Flags = 1 << iota
	FlagNoAutoStart
)

// Type represents the possible types of a DBus message.
type Type byte

const (
	TypeMethodCall Type = 1 + iota
	TypeMethodReply
	TypeError
	TypeSignal
	typeMax
)

// HeaderField represents the possible byte codes for the headers
// of a DBus message.
type HeaderField byte

const (
	FieldPath HeaderField = 1 + iota
	FieldInterface
	FieldMember
	FieldErrorName
	FieldReplySerial
	FieldDestination
	FieldSender
	FieldSignature
	FieldUnixFDs
	fieldMax
)

// An InvalidMessageError describes the reason why a DBus message is regarded as
// invalid.
type InvalidMessageError string

func (e InvalidMessageError) Error() string {
	return "invalid message: " + string(e)
}

// fieldType are the types of the various header fields.
var fieldTypes = [fieldMax]reflect.Type{
	FieldPath:        objectPathType,
	FieldInterface:   stringType,
	FieldMember:      stringType,
	FieldErrorName:   stringType,
	FieldReplySerial: uint32Type,
	FieldDestination: stringType,
	FieldSender:      stringType,
	FieldSignature:   signatureType,
	FieldUnixFDs:     uint32Type,
}

// requiredFields lists the header fields that are required by the different
// message types.
var requiredFields = [typeMax][]HeaderField{
	TypeMethodCall:  {FieldPath, FieldMember},
	TypeMethodReply: {FieldReplySerial},
	TypeError:       {FieldErrorName, FieldReplySerial},
	TypeSignal:      {FieldPath, FieldInterface, FieldMember},
}

// Message represents a single DBus message.
type Message struct {
	// must be binary.BigEndian or binary.LittleEndian
	Order binary.ByteOrder

	Type
	Flags
	Headers map[HeaderField]Variant

	// The message body. For incoming messages, structs are represented as a
	// slice of empty interfaces.
	Body []interface{}

	serial uint32
}

type header struct {
	HeaderField
	Variant
}

// DecodeMessage tries to decode a single message from the given reader.
// The byte order is figured out from the first byte. The possibly returned
// error can be an error of the underlying reader, an InvalidMessageError or a
// FormatError.
func DecodeMessage(rd io.Reader) (msg *Message, err error) {
	var order binary.ByteOrder
	var hlength, length uint32
	var proto byte
	var headers []header

	b := make([]byte, 1)
	_, err = rd.Read(b)
	if err != nil {
		return
	}
	switch b[0] {
	case 'l':
		order = binary.LittleEndian
	case 'B':
		order = binary.BigEndian
	default:
		return nil, InvalidMessageError("invalid byte order")
	}

	dec := NewDecoder(rd, order)
	dec.pos = 1

	msg = new(Message)
	msg.Order = order
	err = dec.Decode(&msg.Type, &msg.Flags, &proto, &length, &msg.serial)
	if err != nil {
		return nil, err
	}

	// get the header length separately because we need it later
	b = make([]byte, 4)
	_, err = io.ReadFull(rd, b)
	if err != nil {
		return nil, err
	}
	binary.Read(bytes.NewBuffer(b), order, &hlength)
	if hlength+length+16 > 1<<27 {
		return nil, InvalidMessageError("message is too long")
	}
	dec = NewDecoder(io.MultiReader(bytes.NewBuffer(b), rd), order)
	dec.pos = 12
	err = dec.Decode(&headers)
	if err != nil {
		return nil, err
	}

	msg.Headers = make(map[HeaderField]Variant)
	for _, v := range headers {
		msg.Headers[v.HeaderField] = v.Variant
	}

	dec.align(8)
	body := make([]byte, int(length))
	if length != 0 {
		_, err := io.ReadFull(rd, body)
		if err != nil {
			return nil, err
		}
	}

	if err = msg.IsValid(); err != nil {
		return nil, err
	}
	sig, _ := msg.Headers[FieldSignature].value.(Signature)
	if sig.str != "" {
		vs := sig.Values()
		buf := bytes.NewBuffer(body)
		dec = NewDecoder(buf, order)
		if err = dec.Decode(vs...); err != nil {
			return nil, err
		}
		msg.Body = dereferenceAll(vs)
	}

	return
}

// EncodeTo encodes and sends a message to the given writer. If the message is
// not valid or an error occurs when writing, an error is returned.
func (msg *Message) EncodeTo(out io.Writer) error {
	if err := msg.IsValid(); err != nil {
		return err
	}
	var vs [7]interface{}
	switch msg.Order {
	case binary.LittleEndian:
		vs[0] = byte('l')
	case binary.BigEndian:
		vs[0] = byte('B')
	}
	body := new(bytes.Buffer)
	enc := NewEncoder(body, msg.Order)
	if len(msg.Body) != 0 {
		enc.Encode(msg.Body...)
	}
	vs[1] = msg.Type
	vs[2] = msg.Flags
	vs[3] = protoVersion
	vs[4] = uint32(len(body.Bytes()))
	vs[5] = msg.serial
	headers := make([]header, 0, len(msg.Headers))
	for k, v := range msg.Headers {
		headers = append(headers, header{k, v})
	}
	vs[6] = headers
	var buf bytes.Buffer
	enc = NewEncoder(&buf, msg.Order)
	enc.Encode(vs[:]...)
	enc.align(8)
	body.WriteTo(&buf)
	if buf.Len() > 1<<27 {
		return InvalidMessageError("message is too long")
	}
	if _, err := buf.WriteTo(out); err != nil {
		return err
	}
	return nil
}

// IsValid checks whether msg is a valid message and returns an
// InvalidMessageError if it is not.
func (msg *Message) IsValid() error {
	switch msg.Order {
	case binary.LittleEndian, binary.BigEndian:
	default:
		return InvalidMessageError("invalid byte order")
	}
	if msg.Flags & ^(FlagNoAutoStart|FlagNoReplyExpected) != 0 {
		return InvalidMessageError("invalid flags")
	}
	if msg.Type == 0 || msg.Type >= typeMax {
		return InvalidMessageError("invalid message type")
	}
	for k, v := range msg.Headers {
		if k == 0 || k >= fieldMax {
			return InvalidMessageError("invalid header")
		}
		if reflect.TypeOf(v.value) != fieldTypes[k] {
			return InvalidMessageError("invalid type of header field")
		}
	}
	for _, v := range requiredFields[msg.Type] {
		if _, ok := msg.Headers[v]; !ok {
			return InvalidMessageError("missing required header")
		}
	}
	if path, ok := msg.Headers[FieldPath]; ok {
		if !path.value.(ObjectPath).IsValid() {
			return InvalidMessageError("invalid path name")
		}
	}
	if iface, ok := msg.Headers[FieldInterface]; ok {
		if !isValidInterface(iface.value.(string)) {
			return InvalidMessageError("invalid interface name")
		}
	}
	if member, ok := msg.Headers[FieldMember]; ok {
		if !isValidMember(member.value.(string)) {
			return InvalidMessageError("invalid member name")
		}
	}
	if errname, ok := msg.Headers[FieldErrorName]; ok {
		if !isValidInterface(errname.value.(string)) {
			return InvalidMessageError("invalid error name")
		}
	}
	if len(msg.Body) != 0 {
		if _, ok := msg.Headers[FieldSignature]; !ok {
			return InvalidMessageError("missing signature")
		}
	}
	return nil
}

// Serial returns the message's serial number. The returned value is only valid
// for messages received by eavesdropping.
func (msg *Message) Serial() uint32 {
	return msg.serial
}

// String returns a string representation of a message similar to the format of
// dbus-monitor.
func (msg *Message) String() string {
	if err := msg.IsValid(); err != nil {
		return "<invalid>"
	}
	s := map[Type]string{
		TypeMethodCall:  "method call",
		TypeMethodReply: "reply",
		TypeError:       "error",
		TypeSignal:      "signal",
	}[msg.Type]
	if v, ok := msg.Headers[FieldSender]; ok {
		s += " from " + v.value.(string)
	}
	if v, ok := msg.Headers[FieldDestination]; ok {
		s += " to " + v.value.(string)
	} else {
		s += " to <null>"
	}
	s += " serial " + strconv.FormatUint(uint64(msg.serial), 10)
	if v, ok := msg.Headers[FieldUnixFDs]; ok {
		s += " unixfds " + strconv.FormatUint(uint64(v.value.(uint32)), 10)
	}
	if v, ok := msg.Headers[FieldPath]; ok {
		s += " path " + string(v.value.(ObjectPath))
	}
	if v, ok := msg.Headers[FieldInterface]; ok {
		s += " interface " + v.value.(string)
	}
	if v, ok := msg.Headers[FieldErrorName]; ok {
		s += " name " + v.value.(string)
	}
	if v, ok := msg.Headers[FieldMember]; ok {
		s += " member " + v.value.(string)
	}
	if len(msg.Body) != 0 {
		s += "\n"
	}
	for i, v := range msg.Body {
		s += "  " + fmt.Sprint(v)
		if i != len(msg.Body)-1 {
			s += "\n"
		}
	}
	return s
}
