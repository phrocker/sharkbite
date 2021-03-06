/**
 * Autogenerated by Thrift Compiler (0.12.0)
 *
 * DO NOT EDIT UNLESS YOU ARE SURE THAT YOU KNOW WHAT YOU ARE DOING
 *  @generated
 */
#ifndef ThriftTest_V2H
#define ThriftTest_V2H

#include <thrift/TDispatchProcessor.h>
#include <thrift/async/TConcurrentClientSyncInfo.h>
#include "client_types.h"

namespace org { namespace apache { namespace accumulov2 { namespace core { namespace clientImpl { namespace thrift {

#ifdef _MSC_VER
  #pragma warning( push )
  #pragma warning (disable : 4250 ) //inheriting methods via dominance 
#endif

class ThriftTestIf {
 public:
  virtual ~ThriftTestIf() {}
  virtual bool success() = 0;
  virtual bool fails() = 0;
  virtual bool throwsError() = 0;
};

class ThriftTestIfFactory {
 public:
  typedef ThriftTestIf Handler;

  virtual ~ThriftTestIfFactory() {}

  virtual ThriftTestIf* getHandler(const ::apache::thrift::TConnectionInfo& connInfo) = 0;
  virtual void releaseHandler(ThriftTestIf* /* handler */) = 0;
};

class ThriftTestIfSingletonFactory : virtual public ThriftTestIfFactory {
 public:
  ThriftTestIfSingletonFactory(const std::shared_ptr<ThriftTestIf>& iface) : iface_(iface) {}
  virtual ~ThriftTestIfSingletonFactory() {}

  virtual ThriftTestIf* getHandler(const ::apache::thrift::TConnectionInfo&) {
    return iface_.get();
  }
  virtual void releaseHandler(ThriftTestIf* /* handler */) {}

 protected:
  std::shared_ptr<ThriftTestIf> iface_;
};

class ThriftTestNull : virtual public ThriftTestIf {
 public:
  virtual ~ThriftTestNull() {}
  bool success() {
    bool _return = false;
    return _return;
  }
  bool fails() {
    bool _return = false;
    return _return;
  }
  bool throwsError() {
    bool _return = false;
    return _return;
  }
};


class ThriftTest_success_args {
 public:

  ThriftTest_success_args(const ThriftTest_success_args&);
  ThriftTest_success_args& operator=(const ThriftTest_success_args&);
  ThriftTest_success_args() {
  }

  virtual ~ThriftTest_success_args() throw();

  bool operator == (const ThriftTest_success_args & /* rhs */) const
  {
    return true;
  }
  bool operator != (const ThriftTest_success_args &rhs) const {
    return !(*this == rhs);
  }

  bool operator < (const ThriftTest_success_args & ) const;

  uint32_t read(::apache::thrift::protocol::TProtocol* iprot);
  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};


class ThriftTest_success_pargs {
 public:


  virtual ~ThriftTest_success_pargs() throw();

  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};

typedef struct _ThriftTest_success_result__isset {
  _ThriftTest_success_result__isset() : success(false) {}
  bool success :1;
} _ThriftTest_success_result__isset;

class ThriftTest_success_result {
 public:

  ThriftTest_success_result(const ThriftTest_success_result&);
  ThriftTest_success_result& operator=(const ThriftTest_success_result&);
  ThriftTest_success_result() : success(0) {
  }

  virtual ~ThriftTest_success_result() throw();
  bool success;

  _ThriftTest_success_result__isset __isset;

  void __set_success(const bool val);

  bool operator == (const ThriftTest_success_result & rhs) const
  {
    if (!(success == rhs.success))
      return false;
    return true;
  }
  bool operator != (const ThriftTest_success_result &rhs) const {
    return !(*this == rhs);
  }

  bool operator < (const ThriftTest_success_result & ) const;

  uint32_t read(::apache::thrift::protocol::TProtocol* iprot);
  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};

typedef struct _ThriftTest_success_presult__isset {
  _ThriftTest_success_presult__isset() : success(false) {}
  bool success :1;
} _ThriftTest_success_presult__isset;

class ThriftTest_success_presult {
 public:


  virtual ~ThriftTest_success_presult() throw();
  bool* success;

  _ThriftTest_success_presult__isset __isset;

  uint32_t read(::apache::thrift::protocol::TProtocol* iprot);

};


class ThriftTest_fails_args {
 public:

  ThriftTest_fails_args(const ThriftTest_fails_args&);
  ThriftTest_fails_args& operator=(const ThriftTest_fails_args&);
  ThriftTest_fails_args() {
  }

  virtual ~ThriftTest_fails_args() throw();

  bool operator == (const ThriftTest_fails_args & /* rhs */) const
  {
    return true;
  }
  bool operator != (const ThriftTest_fails_args &rhs) const {
    return !(*this == rhs);
  }

  bool operator < (const ThriftTest_fails_args & ) const;

  uint32_t read(::apache::thrift::protocol::TProtocol* iprot);
  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};


class ThriftTest_fails_pargs {
 public:


  virtual ~ThriftTest_fails_pargs() throw();

  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};

typedef struct _ThriftTest_fails_result__isset {
  _ThriftTest_fails_result__isset() : success(false) {}
  bool success :1;
} _ThriftTest_fails_result__isset;

class ThriftTest_fails_result {
 public:

  ThriftTest_fails_result(const ThriftTest_fails_result&);
  ThriftTest_fails_result& operator=(const ThriftTest_fails_result&);
  ThriftTest_fails_result() : success(0) {
  }

  virtual ~ThriftTest_fails_result() throw();
  bool success;

  _ThriftTest_fails_result__isset __isset;

  void __set_success(const bool val);

  bool operator == (const ThriftTest_fails_result & rhs) const
  {
    if (!(success == rhs.success))
      return false;
    return true;
  }
  bool operator != (const ThriftTest_fails_result &rhs) const {
    return !(*this == rhs);
  }

  bool operator < (const ThriftTest_fails_result & ) const;

  uint32_t read(::apache::thrift::protocol::TProtocol* iprot);
  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};

typedef struct _ThriftTest_fails_presult__isset {
  _ThriftTest_fails_presult__isset() : success(false) {}
  bool success :1;
} _ThriftTest_fails_presult__isset;

class ThriftTest_fails_presult {
 public:


  virtual ~ThriftTest_fails_presult() throw();
  bool* success;

  _ThriftTest_fails_presult__isset __isset;

  uint32_t read(::apache::thrift::protocol::TProtocol* iprot);

};


class ThriftTest_throwsError_args {
 public:

  ThriftTest_throwsError_args(const ThriftTest_throwsError_args&);
  ThriftTest_throwsError_args& operator=(const ThriftTest_throwsError_args&);
  ThriftTest_throwsError_args() {
  }

  virtual ~ThriftTest_throwsError_args() throw();

  bool operator == (const ThriftTest_throwsError_args & /* rhs */) const
  {
    return true;
  }
  bool operator != (const ThriftTest_throwsError_args &rhs) const {
    return !(*this == rhs);
  }

  bool operator < (const ThriftTest_throwsError_args & ) const;

  uint32_t read(::apache::thrift::protocol::TProtocol* iprot);
  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};


class ThriftTest_throwsError_pargs {
 public:


  virtual ~ThriftTest_throwsError_pargs() throw();

  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};

typedef struct _ThriftTest_throwsError_result__isset {
  _ThriftTest_throwsError_result__isset() : success(false), ex(false) {}
  bool success :1;
  bool ex :1;
} _ThriftTest_throwsError_result__isset;

class ThriftTest_throwsError_result {
 public:

  ThriftTest_throwsError_result(const ThriftTest_throwsError_result&);
  ThriftTest_throwsError_result& operator=(const ThriftTest_throwsError_result&);
  ThriftTest_throwsError_result() : success(0) {
  }

  virtual ~ThriftTest_throwsError_result() throw();
  bool success;
  ThriftSecurityException ex;

  _ThriftTest_throwsError_result__isset __isset;

  void __set_success(const bool val);

  void __set_ex(const ThriftSecurityException& val);

  bool operator == (const ThriftTest_throwsError_result & rhs) const
  {
    if (!(success == rhs.success))
      return false;
    if (!(ex == rhs.ex))
      return false;
    return true;
  }
  bool operator != (const ThriftTest_throwsError_result &rhs) const {
    return !(*this == rhs);
  }

  bool operator < (const ThriftTest_throwsError_result & ) const;

  uint32_t read(::apache::thrift::protocol::TProtocol* iprot);
  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};

typedef struct _ThriftTest_throwsError_presult__isset {
  _ThriftTest_throwsError_presult__isset() : success(false), ex(false) {}
  bool success :1;
  bool ex :1;
} _ThriftTest_throwsError_presult__isset;

class ThriftTest_throwsError_presult {
 public:


  virtual ~ThriftTest_throwsError_presult() throw();
  bool* success;
  ThriftSecurityException ex;

  _ThriftTest_throwsError_presult__isset __isset;

  uint32_t read(::apache::thrift::protocol::TProtocol* iprot);

};

class ThriftTestClient : virtual public ThriftTestIf {
 public:
  ThriftTestClient(std::shared_ptr< ::apache::thrift::protocol::TProtocol> prot) {
    setProtocol(prot);
  }
  ThriftTestClient(std::shared_ptr< ::apache::thrift::protocol::TProtocol> iprot, std::shared_ptr< ::apache::thrift::protocol::TProtocol> oprot) {
    setProtocol(iprot,oprot);
  }
 private:
  void setProtocol(std::shared_ptr< ::apache::thrift::protocol::TProtocol> prot) {
  setProtocol(prot,prot);
  }
  void setProtocol(std::shared_ptr< ::apache::thrift::protocol::TProtocol> iprot, std::shared_ptr< ::apache::thrift::protocol::TProtocol> oprot) {
    piprot_=iprot;
    poprot_=oprot;
    iprot_ = iprot.get();
    oprot_ = oprot.get();
  }
 public:
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> getInputProtocol() {
    return piprot_;
  }
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> getOutputProtocol() {
    return poprot_;
  }
  bool success();
  void send_success();
  bool recv_success();
  bool fails();
  void send_fails();
  bool recv_fails();
  bool throwsError();
  void send_throwsError();
  bool recv_throwsError();
 protected:
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> piprot_;
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> poprot_;
  ::apache::thrift::protocol::TProtocol* iprot_;
  ::apache::thrift::protocol::TProtocol* oprot_;
};

class ThriftTestProcessor : public ::apache::thrift::TDispatchProcessor {
 protected:
  std::shared_ptr<ThriftTestIf> iface_;
  virtual bool dispatchCall(::apache::thrift::protocol::TProtocol* iprot, ::apache::thrift::protocol::TProtocol* oprot, const std::string& fname, int32_t seqid, void* callContext);
 private:
  typedef  void (ThriftTestProcessor::*ProcessFunction)(int32_t, ::apache::thrift::protocol::TProtocol*, ::apache::thrift::protocol::TProtocol*, void*);
  typedef std::map<std::string, ProcessFunction> ProcessMap;
  ProcessMap processMap_;
  void process_success(int32_t seqid, ::apache::thrift::protocol::TProtocol* iprot, ::apache::thrift::protocol::TProtocol* oprot, void* callContext);
  void process_fails(int32_t seqid, ::apache::thrift::protocol::TProtocol* iprot, ::apache::thrift::protocol::TProtocol* oprot, void* callContext);
  void process_throwsError(int32_t seqid, ::apache::thrift::protocol::TProtocol* iprot, ::apache::thrift::protocol::TProtocol* oprot, void* callContext);
 public:
  ThriftTestProcessor(std::shared_ptr<ThriftTestIf> iface) :
    iface_(iface) {
    processMap_["success"] = &ThriftTestProcessor::process_success;
    processMap_["fails"] = &ThriftTestProcessor::process_fails;
    processMap_["throwsError"] = &ThriftTestProcessor::process_throwsError;
  }

  virtual ~ThriftTestProcessor() {}
};

class ThriftTestProcessorFactory : public ::apache::thrift::TProcessorFactory {
 public:
  ThriftTestProcessorFactory(const std::shared_ptr< ThriftTestIfFactory >& handlerFactory) :
      handlerFactory_(handlerFactory) {}

  std::shared_ptr< ::apache::thrift::TProcessor > getProcessor(const ::apache::thrift::TConnectionInfo& connInfo);

 protected:
  std::shared_ptr< ThriftTestIfFactory > handlerFactory_;
};

class ThriftTestMultiface : virtual public ThriftTestIf {
 public:
  ThriftTestMultiface(std::vector<std::shared_ptr<ThriftTestIf> >& ifaces) : ifaces_(ifaces) {
  }
  virtual ~ThriftTestMultiface() {}
 protected:
  std::vector<std::shared_ptr<ThriftTestIf> > ifaces_;
  ThriftTestMultiface() {}
  void add(std::shared_ptr<ThriftTestIf> iface) {
    ifaces_.push_back(iface);
  }
 public:
  bool success() {
    size_t sz = ifaces_.size();
    size_t i = 0;
    for (; i < (sz - 1); ++i) {
      ifaces_[i]->success();
    }
    return ifaces_[i]->success();
  }

  bool fails() {
    size_t sz = ifaces_.size();
    size_t i = 0;
    for (; i < (sz - 1); ++i) {
      ifaces_[i]->fails();
    }
    return ifaces_[i]->fails();
  }

  bool throwsError() {
    size_t sz = ifaces_.size();
    size_t i = 0;
    for (; i < (sz - 1); ++i) {
      ifaces_[i]->throwsError();
    }
    return ifaces_[i]->throwsError();
  }

};

// The 'concurrent' client is a thread safe client that correctly handles
// out of order responses.  It is slower than the regular client, so should
// only be used when you need to share a connection among multiple threads
class ThriftTestConcurrentClient : virtual public ThriftTestIf {
 public:
  ThriftTestConcurrentClient(std::shared_ptr< ::apache::thrift::protocol::TProtocol> prot) {
    setProtocol(prot);
  }
  ThriftTestConcurrentClient(std::shared_ptr< ::apache::thrift::protocol::TProtocol> iprot, std::shared_ptr< ::apache::thrift::protocol::TProtocol> oprot) {
    setProtocol(iprot,oprot);
  }
 private:
  void setProtocol(std::shared_ptr< ::apache::thrift::protocol::TProtocol> prot) {
  setProtocol(prot,prot);
  }
  void setProtocol(std::shared_ptr< ::apache::thrift::protocol::TProtocol> iprot, std::shared_ptr< ::apache::thrift::protocol::TProtocol> oprot) {
    piprot_=iprot;
    poprot_=oprot;
    iprot_ = iprot.get();
    oprot_ = oprot.get();
  }
 public:
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> getInputProtocol() {
    return piprot_;
  }
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> getOutputProtocol() {
    return poprot_;
  }
  bool success();
  int32_t send_success();
  bool recv_success(const int32_t seqid);
  bool fails();
  int32_t send_fails();
  bool recv_fails(const int32_t seqid);
  bool throwsError();
  int32_t send_throwsError();
  bool recv_throwsError(const int32_t seqid);
 protected:
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> piprot_;
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> poprot_;
  ::apache::thrift::protocol::TProtocol* iprot_;
  ::apache::thrift::protocol::TProtocol* oprot_;
  ::apache::thrift::async::TConcurrentClientSyncInfo sync_;
};

#ifdef _MSC_VER
  #pragma warning( pop )
#endif

}}}}}} // namespace

#endif
