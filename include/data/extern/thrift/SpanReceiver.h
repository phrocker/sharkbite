/**
 * Autogenerated by Thrift Compiler (0.9.3)
 *
 * DO NOT EDIT UNLESS YOU ARE SURE THAT YOU KNOW WHAT YOU ARE DOING
 *  @generated
 */
#ifndef SpanReceiver_H
#define SpanReceiver_H

#include <thrift/TDispatchProcessor.h>
#include <thrift/async/TConcurrentClientSyncInfo.h>
#include "tracer_types.h"

namespace org { namespace apache { namespace accumulo { namespace tracer { namespace thrift {

#ifdef _WIN32
  #pragma warning( push )
  #pragma warning (disable : 4250 ) //inheriting methods via dominance 
#endif

class SpanReceiverIf {
 public:
  virtual ~SpanReceiverIf() {}
  virtual void span(const RemoteSpan& span) = 0;
};

class SpanReceiverIfFactory {
 public:
  typedef SpanReceiverIf Handler;

  virtual ~SpanReceiverIfFactory() {}

  virtual SpanReceiverIf* getHandler(const ::apache::thrift::TConnectionInfo& connInfo) = 0;
  virtual void releaseHandler(SpanReceiverIf* /* handler */) = 0;
};

class SpanReceiverIfSingletonFactory : virtual public SpanReceiverIfFactory {
 public:
  SpanReceiverIfSingletonFactory(const std::shared_ptr<SpanReceiverIf>& iface) : iface_(iface) {}
  virtual ~SpanReceiverIfSingletonFactory() {}

  virtual SpanReceiverIf* getHandler(const ::apache::thrift::TConnectionInfo&) {
    return iface_.get();
  }
  virtual void releaseHandler(SpanReceiverIf* /* handler */) {}

 protected:
  std::shared_ptr<SpanReceiverIf> iface_;
};

class SpanReceiverNull : virtual public SpanReceiverIf {
 public:
  virtual ~SpanReceiverNull() {}
  void span(const RemoteSpan& /* span */) {
    return;
  }
};

typedef struct _SpanReceiver_span_args__isset {
  _SpanReceiver_span_args__isset() : span(false) {}
  bool span :1;
} _SpanReceiver_span_args__isset;

class SpanReceiver_span_args {
 public:

  SpanReceiver_span_args(const SpanReceiver_span_args&);
  SpanReceiver_span_args& operator=(const SpanReceiver_span_args&);
  SpanReceiver_span_args() {
  }

  virtual ~SpanReceiver_span_args() throw();
  RemoteSpan span;

  _SpanReceiver_span_args__isset __isset;

  void __set_span(const RemoteSpan& val);

  bool operator == (const SpanReceiver_span_args & rhs) const
  {
    if (!(span == rhs.span))
      return false;
    return true;
  }
  bool operator != (const SpanReceiver_span_args &rhs) const {
    return !(*this == rhs);
  }

  bool operator < (const SpanReceiver_span_args & ) const;

  uint32_t read(::apache::thrift::protocol::TProtocol* iprot);
  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};


class SpanReceiver_span_pargs {
 public:


  virtual ~SpanReceiver_span_pargs() throw();
  const RemoteSpan* span;

  uint32_t write(::apache::thrift::protocol::TProtocol* oprot) const;

};

class SpanReceiverClient : virtual public SpanReceiverIf {
 public:
  SpanReceiverClient(std::shared_ptr< ::apache::thrift::protocol::TProtocol> prot) {
    setProtocol(prot);
  }
  SpanReceiverClient(std::shared_ptr< ::apache::thrift::protocol::TProtocol> iprot, std::shared_ptr< ::apache::thrift::protocol::TProtocol> oprot) {
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
  void span(const RemoteSpan& span);
  void send_span(const RemoteSpan& span);
 protected:
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> piprot_;
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> poprot_;
  ::apache::thrift::protocol::TProtocol* iprot_;
  ::apache::thrift::protocol::TProtocol* oprot_;
};

class SpanReceiverProcessor : public ::apache::thrift::TDispatchProcessor {
 protected:
  std::shared_ptr<SpanReceiverIf> iface_;
  virtual bool dispatchCall(::apache::thrift::protocol::TProtocol* iprot, ::apache::thrift::protocol::TProtocol* oprot, const std::string& fname, int32_t seqid, void* callContext);
 private:
  typedef  void (SpanReceiverProcessor::*ProcessFunction)(int32_t, ::apache::thrift::protocol::TProtocol*, ::apache::thrift::protocol::TProtocol*, void*);
  typedef std::map<std::string, ProcessFunction> ProcessMap;
  ProcessMap processMap_;
  void process_span(int32_t seqid, ::apache::thrift::protocol::TProtocol* iprot, ::apache::thrift::protocol::TProtocol* oprot, void* callContext);
 public:
  SpanReceiverProcessor(std::shared_ptr<SpanReceiverIf> iface) :
    iface_(iface) {
    processMap_["span"] = &SpanReceiverProcessor::process_span;
  }

  virtual ~SpanReceiverProcessor() {}
};

class SpanReceiverProcessorFactory : public ::apache::thrift::TProcessorFactory {
 public:
  SpanReceiverProcessorFactory(const ::std::shared_ptr< SpanReceiverIfFactory >& handlerFactory) :
      handlerFactory_(handlerFactory) {}

  ::std::shared_ptr< ::apache::thrift::TProcessor > getProcessor(const ::apache::thrift::TConnectionInfo& connInfo);

 protected:
  ::std::shared_ptr< SpanReceiverIfFactory > handlerFactory_;
};

class SpanReceiverMultiface : virtual public SpanReceiverIf {
 public:
  SpanReceiverMultiface(std::vector<std::shared_ptr<SpanReceiverIf> >& ifaces) : ifaces_(ifaces) {
  }
  virtual ~SpanReceiverMultiface() {}
 protected:
  std::vector<std::shared_ptr<SpanReceiverIf> > ifaces_;
  SpanReceiverMultiface() {}
  void add(std::shared_ptr<SpanReceiverIf> iface) {
    ifaces_.push_back(iface);
  }
 public:
  void span(const RemoteSpan& span) {
    size_t sz = ifaces_.size();
    size_t i = 0;
    for (; i < (sz - 1); ++i) {
      ifaces_[i]->span(span);
    }
    ifaces_[i]->span(span);
  }

};

// The 'concurrent' client is a thread safe client that correctly handles
// out of order responses.  It is slower than the regular client, so should
// only be used when you need to share a connection among multiple threads
class SpanReceiverConcurrentClient : virtual public SpanReceiverIf {
 public:
  SpanReceiverConcurrentClient(std::shared_ptr< ::apache::thrift::protocol::TProtocol> prot) {
    setProtocol(prot);
  }
  SpanReceiverConcurrentClient(std::shared_ptr< ::apache::thrift::protocol::TProtocol> iprot, std::shared_ptr< ::apache::thrift::protocol::TProtocol> oprot) {
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
  void span(const RemoteSpan& span);
  void send_span(const RemoteSpan& span);
 protected:
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> piprot_;
  std::shared_ptr< ::apache::thrift::protocol::TProtocol> poprot_;
  ::apache::thrift::protocol::TProtocol* iprot_;
  ::apache::thrift::protocol::TProtocol* oprot_;
  ::apache::thrift::async::TConcurrentClientSyncInfo sync_;
};

#ifdef _WIN32
  #pragma warning( pop )
#endif

}}}}} // namespace

#endif
