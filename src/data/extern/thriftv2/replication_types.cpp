/**
 * Autogenerated by Thrift Compiler (0.12.0)
 *
 * DO NOT EDIT UNLESS YOU ARE SURE THAT YOU KNOW WHAT YOU ARE DOING
 *  @generated
 */
#include "data/extern/thriftv2/replication_types.h"

#include <algorithm>
#include <ostream>

#include <thrift/TToString.h>

namespace org { namespace apache { namespace accumulov2 { namespace core { namespace replication { namespace thrift {

int _kRemoteReplicationErrorCodeValues[] = {
  RemoteReplicationErrorCode::COULD_NOT_DESERIALIZE,
  RemoteReplicationErrorCode::COULD_NOT_APPLY,
  RemoteReplicationErrorCode::TABLE_DOES_NOT_EXIST,
  RemoteReplicationErrorCode::CANNOT_AUTHENTICATE,
  RemoteReplicationErrorCode::CANNOT_INSTANTIATE_REPLAYER
};
const char* _kRemoteReplicationErrorCodeNames[] = {
  "COULD_NOT_DESERIALIZE",
  "COULD_NOT_APPLY",
  "TABLE_DOES_NOT_EXIST",
  "CANNOT_AUTHENTICATE",
  "CANNOT_INSTANTIATE_REPLAYER"
};
const std::map<int, const char*> _RemoteReplicationErrorCode_VALUES_TO_NAMES(::apache::thrift::TEnumIterator(5, _kRemoteReplicationErrorCodeValues, _kRemoteReplicationErrorCodeNames), ::apache::thrift::TEnumIterator(-1, NULL, NULL));

std::ostream& operator<<(std::ostream& out, const RemoteReplicationErrorCode::type& val) {
  std::map<int, const char*>::const_iterator it = _RemoteReplicationErrorCode_VALUES_TO_NAMES.find(val);
  if (it != _RemoteReplicationErrorCode_VALUES_TO_NAMES.end()) {
    out << it->second;
  } else {
    out << static_cast<int>(val);
  }
  return out;
}

int _kReplicationCoordinatorErrorCodeValues[] = {
  ReplicationCoordinatorErrorCode::NO_AVAILABLE_SERVERS,
  ReplicationCoordinatorErrorCode::SERVICE_CONFIGURATION_UNAVAILABLE,
  ReplicationCoordinatorErrorCode::CANNOT_AUTHENTICATE
};
const char* _kReplicationCoordinatorErrorCodeNames[] = {
  "NO_AVAILABLE_SERVERS",
  "SERVICE_CONFIGURATION_UNAVAILABLE",
  "CANNOT_AUTHENTICATE"
};
const std::map<int, const char*> _ReplicationCoordinatorErrorCode_VALUES_TO_NAMES(::apache::thrift::TEnumIterator(3, _kReplicationCoordinatorErrorCodeValues, _kReplicationCoordinatorErrorCodeNames), ::apache::thrift::TEnumIterator(-1, NULL, NULL));

std::ostream& operator<<(std::ostream& out, const ReplicationCoordinatorErrorCode::type& val) {
  std::map<int, const char*>::const_iterator it = _ReplicationCoordinatorErrorCode_VALUES_TO_NAMES.find(val);
  if (it != _ReplicationCoordinatorErrorCode_VALUES_TO_NAMES.end()) {
    out << it->second;
  } else {
    out << static_cast<int>(val);
  }
  return out;
}


WalEdits::~WalEdits() throw() {
}


void WalEdits::__set_edits(const std::vector<std::string> & val) {
  this->edits = val;
}
std::ostream& operator<<(std::ostream& out, const WalEdits& obj)
{
  obj.printTo(out);
  return out;
}


uint32_t WalEdits::read(::apache::thrift::protocol::TProtocol* iprot) {

  ::apache::thrift::protocol::TInputRecursionTracker tracker(*iprot);
  uint32_t xfer = 0;
  std::string fname;
  ::apache::thrift::protocol::TType ftype;
  int16_t fid;

  xfer += iprot->readStructBegin(fname);

  using ::apache::thrift::protocol::TProtocolException;


  while (true)
  {
    xfer += iprot->readFieldBegin(fname, ftype, fid);
    if (ftype == ::apache::thrift::protocol::T_STOP) {
      break;
    }
    switch (fid)
    {
      case 1:
        if (ftype == ::apache::thrift::protocol::T_LIST) {
          {
            this->edits.clear();
            uint32_t _size0;
            ::apache::thrift::protocol::TType _etype3;
            xfer += iprot->readListBegin(_etype3, _size0);
            this->edits.resize(_size0);
            uint32_t _i4;
            for (_i4 = 0; _i4 < _size0; ++_i4)
            {
              xfer += iprot->readBinary(this->edits[_i4]);
            }
            xfer += iprot->readListEnd();
          }
          this->__isset.edits = true;
        } else {
          xfer += iprot->skip(ftype);
        }
        break;
      default:
        xfer += iprot->skip(ftype);
        break;
    }
    xfer += iprot->readFieldEnd();
  }

  xfer += iprot->readStructEnd();

  return xfer;
}

uint32_t WalEdits::write(::apache::thrift::protocol::TProtocol* oprot) const {
  uint32_t xfer = 0;
  ::apache::thrift::protocol::TOutputRecursionTracker tracker(*oprot);
  xfer += oprot->writeStructBegin("WalEdits");

  xfer += oprot->writeFieldBegin("edits", ::apache::thrift::protocol::T_LIST, 1);
  {
    xfer += oprot->writeListBegin(::apache::thrift::protocol::T_STRING, static_cast<uint32_t>(this->edits.size()));
    std::vector<std::string> ::const_iterator _iter5;
    for (_iter5 = this->edits.begin(); _iter5 != this->edits.end(); ++_iter5)
    {
      xfer += oprot->writeBinary((*_iter5));
    }
    xfer += oprot->writeListEnd();
  }
  xfer += oprot->writeFieldEnd();

  xfer += oprot->writeFieldStop();
  xfer += oprot->writeStructEnd();
  return xfer;
}

void swap(WalEdits &a, WalEdits &b) {
  using ::std::swap;
  swap(a.edits, b.edits);
  swap(a.__isset, b.__isset);
}

WalEdits::WalEdits(const WalEdits& other6) {
  edits = other6.edits;
  __isset = other6.__isset;
}
WalEdits& WalEdits::operator=(const WalEdits& other7) {
  edits = other7.edits;
  __isset = other7.__isset;
  return *this;
}
void WalEdits::printTo(std::ostream& out) const {
  using ::apache::thrift::to_string;
  out << "WalEdits(";
  out << "edits=" << to_string(edits);
  out << ")";
}


KeyValues::~KeyValues() throw() {
}


void KeyValues::__set_keyValues(const std::vector< ::org::apache::accumulov2::core::dataImpl::thrift::TKeyValue> & val) {
  this->keyValues = val;
}
std::ostream& operator<<(std::ostream& out, const KeyValues& obj)
{
  obj.printTo(out);
  return out;
}


uint32_t KeyValues::read(::apache::thrift::protocol::TProtocol* iprot) {

  ::apache::thrift::protocol::TInputRecursionTracker tracker(*iprot);
  uint32_t xfer = 0;
  std::string fname;
  ::apache::thrift::protocol::TType ftype;
  int16_t fid;

  xfer += iprot->readStructBegin(fname);

  using ::apache::thrift::protocol::TProtocolException;


  while (true)
  {
    xfer += iprot->readFieldBegin(fname, ftype, fid);
    if (ftype == ::apache::thrift::protocol::T_STOP) {
      break;
    }
    switch (fid)
    {
      case 1:
        if (ftype == ::apache::thrift::protocol::T_LIST) {
          {
            this->keyValues.clear();
            uint32_t _size8;
            ::apache::thrift::protocol::TType _etype11;
            xfer += iprot->readListBegin(_etype11, _size8);
            this->keyValues.resize(_size8);
            uint32_t _i12;
            for (_i12 = 0; _i12 < _size8; ++_i12)
            {
              xfer += this->keyValues[_i12].read(iprot);
            }
            xfer += iprot->readListEnd();
          }
          this->__isset.keyValues = true;
        } else {
          xfer += iprot->skip(ftype);
        }
        break;
      default:
        xfer += iprot->skip(ftype);
        break;
    }
    xfer += iprot->readFieldEnd();
  }

  xfer += iprot->readStructEnd();

  return xfer;
}

uint32_t KeyValues::write(::apache::thrift::protocol::TProtocol* oprot) const {
  uint32_t xfer = 0;
  ::apache::thrift::protocol::TOutputRecursionTracker tracker(*oprot);
  xfer += oprot->writeStructBegin("KeyValues");

  xfer += oprot->writeFieldBegin("keyValues", ::apache::thrift::protocol::T_LIST, 1);
  {
    xfer += oprot->writeListBegin(::apache::thrift::protocol::T_STRUCT, static_cast<uint32_t>(this->keyValues.size()));
    std::vector< ::org::apache::accumulov2::core::dataImpl::thrift::TKeyValue> ::const_iterator _iter13;
    for (_iter13 = this->keyValues.begin(); _iter13 != this->keyValues.end(); ++_iter13)
    {
      xfer += (*_iter13).write(oprot);
    }
    xfer += oprot->writeListEnd();
  }
  xfer += oprot->writeFieldEnd();

  xfer += oprot->writeFieldStop();
  xfer += oprot->writeStructEnd();
  return xfer;
}

void swap(KeyValues &a, KeyValues &b) {
  using ::std::swap;
  swap(a.keyValues, b.keyValues);
  swap(a.__isset, b.__isset);
}

KeyValues::KeyValues(const KeyValues& other14) {
  keyValues = other14.keyValues;
  __isset = other14.__isset;
}
KeyValues& KeyValues::operator=(const KeyValues& other15) {
  keyValues = other15.keyValues;
  __isset = other15.__isset;
  return *this;
}
void KeyValues::printTo(std::ostream& out) const {
  using ::apache::thrift::to_string;
  out << "KeyValues(";
  out << "keyValues=" << to_string(keyValues);
  out << ")";
}


ReplicationCoordinatorException::~ReplicationCoordinatorException() throw() {
}


void ReplicationCoordinatorException::__set_code(const ReplicationCoordinatorErrorCode::type val) {
  this->code = val;
}

void ReplicationCoordinatorException::__set_reason(const std::string& val) {
  this->reason = val;
}
std::ostream& operator<<(std::ostream& out, const ReplicationCoordinatorException& obj)
{
  obj.printTo(out);
  return out;
}


uint32_t ReplicationCoordinatorException::read(::apache::thrift::protocol::TProtocol* iprot) {

  ::apache::thrift::protocol::TInputRecursionTracker tracker(*iprot);
  uint32_t xfer = 0;
  std::string fname;
  ::apache::thrift::protocol::TType ftype;
  int16_t fid;

  xfer += iprot->readStructBegin(fname);

  using ::apache::thrift::protocol::TProtocolException;


  while (true)
  {
    xfer += iprot->readFieldBegin(fname, ftype, fid);
    if (ftype == ::apache::thrift::protocol::T_STOP) {
      break;
    }
    switch (fid)
    {
      case 1:
        if (ftype == ::apache::thrift::protocol::T_I32) {
          int32_t ecast16;
          xfer += iprot->readI32(ecast16);
          this->code = (ReplicationCoordinatorErrorCode::type)ecast16;
          this->__isset.code = true;
        } else {
          xfer += iprot->skip(ftype);
        }
        break;
      case 2:
        if (ftype == ::apache::thrift::protocol::T_STRING) {
          xfer += iprot->readString(this->reason);
          this->__isset.reason = true;
        } else {
          xfer += iprot->skip(ftype);
        }
        break;
      default:
        xfer += iprot->skip(ftype);
        break;
    }
    xfer += iprot->readFieldEnd();
  }

  xfer += iprot->readStructEnd();

  return xfer;
}

uint32_t ReplicationCoordinatorException::write(::apache::thrift::protocol::TProtocol* oprot) const {
  uint32_t xfer = 0;
  ::apache::thrift::protocol::TOutputRecursionTracker tracker(*oprot);
  xfer += oprot->writeStructBegin("ReplicationCoordinatorException");

  xfer += oprot->writeFieldBegin("code", ::apache::thrift::protocol::T_I32, 1);
  xfer += oprot->writeI32((int32_t)this->code);
  xfer += oprot->writeFieldEnd();

  xfer += oprot->writeFieldBegin("reason", ::apache::thrift::protocol::T_STRING, 2);
  xfer += oprot->writeString(this->reason);
  xfer += oprot->writeFieldEnd();

  xfer += oprot->writeFieldStop();
  xfer += oprot->writeStructEnd();
  return xfer;
}

void swap(ReplicationCoordinatorException &a, ReplicationCoordinatorException &b) {
  using ::std::swap;
  swap(a.code, b.code);
  swap(a.reason, b.reason);
  swap(a.__isset, b.__isset);
}

ReplicationCoordinatorException::ReplicationCoordinatorException(const ReplicationCoordinatorException& other17) : TException() {
  code = other17.code;
  reason = other17.reason;
  __isset = other17.__isset;
}
ReplicationCoordinatorException& ReplicationCoordinatorException::operator=(const ReplicationCoordinatorException& other18) {
  code = other18.code;
  reason = other18.reason;
  __isset = other18.__isset;
  return *this;
}
void ReplicationCoordinatorException::printTo(std::ostream& out) const {
  using ::apache::thrift::to_string;
  out << "ReplicationCoordinatorException(";
  out << "code=" << to_string(code);
  out << ", " << "reason=" << to_string(reason);
  out << ")";
}

const char* ReplicationCoordinatorException::what() const throw() {
  try {
    std::stringstream ss;
    ss << "TException - service has thrown: " << *this;
    this->thriftTExceptionMessageHolder_ = ss.str();
    return this->thriftTExceptionMessageHolder_.c_str();
  } catch (const std::exception&) {
    return "TException - service has thrown: ReplicationCoordinatorException";
  }
}


RemoteReplicationException::~RemoteReplicationException() throw() {
}


void RemoteReplicationException::__set_code(const RemoteReplicationErrorCode::type val) {
  this->code = val;
}

void RemoteReplicationException::__set_reason(const std::string& val) {
  this->reason = val;
}
std::ostream& operator<<(std::ostream& out, const RemoteReplicationException& obj)
{
  obj.printTo(out);
  return out;
}


uint32_t RemoteReplicationException::read(::apache::thrift::protocol::TProtocol* iprot) {

  ::apache::thrift::protocol::TInputRecursionTracker tracker(*iprot);
  uint32_t xfer = 0;
  std::string fname;
  ::apache::thrift::protocol::TType ftype;
  int16_t fid;

  xfer += iprot->readStructBegin(fname);

  using ::apache::thrift::protocol::TProtocolException;


  while (true)
  {
    xfer += iprot->readFieldBegin(fname, ftype, fid);
    if (ftype == ::apache::thrift::protocol::T_STOP) {
      break;
    }
    switch (fid)
    {
      case 1:
        if (ftype == ::apache::thrift::protocol::T_I32) {
          int32_t ecast19;
          xfer += iprot->readI32(ecast19);
          this->code = (RemoteReplicationErrorCode::type)ecast19;
          this->__isset.code = true;
        } else {
          xfer += iprot->skip(ftype);
        }
        break;
      case 2:
        if (ftype == ::apache::thrift::protocol::T_STRING) {
          xfer += iprot->readString(this->reason);
          this->__isset.reason = true;
        } else {
          xfer += iprot->skip(ftype);
        }
        break;
      default:
        xfer += iprot->skip(ftype);
        break;
    }
    xfer += iprot->readFieldEnd();
  }

  xfer += iprot->readStructEnd();

  return xfer;
}

uint32_t RemoteReplicationException::write(::apache::thrift::protocol::TProtocol* oprot) const {
  uint32_t xfer = 0;
  ::apache::thrift::protocol::TOutputRecursionTracker tracker(*oprot);
  xfer += oprot->writeStructBegin("RemoteReplicationException");

  xfer += oprot->writeFieldBegin("code", ::apache::thrift::protocol::T_I32, 1);
  xfer += oprot->writeI32((int32_t)this->code);
  xfer += oprot->writeFieldEnd();

  xfer += oprot->writeFieldBegin("reason", ::apache::thrift::protocol::T_STRING, 2);
  xfer += oprot->writeString(this->reason);
  xfer += oprot->writeFieldEnd();

  xfer += oprot->writeFieldStop();
  xfer += oprot->writeStructEnd();
  return xfer;
}

void swap(RemoteReplicationException &a, RemoteReplicationException &b) {
  using ::std::swap;
  swap(a.code, b.code);
  swap(a.reason, b.reason);
  swap(a.__isset, b.__isset);
}

RemoteReplicationException::RemoteReplicationException(const RemoteReplicationException& other20) : TException() {
  code = other20.code;
  reason = other20.reason;
  __isset = other20.__isset;
}
RemoteReplicationException& RemoteReplicationException::operator=(const RemoteReplicationException& other21) {
  code = other21.code;
  reason = other21.reason;
  __isset = other21.__isset;
  return *this;
}
void RemoteReplicationException::printTo(std::ostream& out) const {
  using ::apache::thrift::to_string;
  out << "RemoteReplicationException(";
  out << "code=" << to_string(code);
  out << ", " << "reason=" << to_string(reason);
  out << ")";
}

const char* RemoteReplicationException::what() const throw() {
  try {
    std::stringstream ss;
    ss << "TException - service has thrown: " << *this;
    this->thriftTExceptionMessageHolder_ = ss.str();
    return this->thriftTExceptionMessageHolder_.c_str();
  } catch (const std::exception&) {
    return "TException - service has thrown: RemoteReplicationException";
  }
}

}}}}}} // namespace
