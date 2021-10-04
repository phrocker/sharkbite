/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#ifndef CACHEDTRANSPORT_H_
#define CACHEDTRANSPORT_H_

#include <memory>
#include <stdexcept>
#include <string>

#include "ServerConnection.h"
#include "Transport.h"
namespace interconnect {

template <typename T>
class CachedTransport {
 public:
  CachedTransport(std::shared_ptr<T> transport,
                  std::shared_ptr<ServerConnection> key)
      : ioCount(0),
        lastCount(-1),
        reserved(false),
        threadName(""),
        foundError(false),
        lastReturnTime(0) {
    srand(time(NULL));
    cacheKey = key;

    serverTransport = transport;
  }

  ~CachedTransport() { close(); }

  void reserve(bool reserve = true) { reserved = reserve; }

  bool isReserved() { return reserved; }

  bool open() {
    try {
      ioCount++;
      serverTransport->open();
    } catch (std::runtime_error &tfe) {
      foundError = true;
      throw;
    }

    ioCount++;

    return (foundError == false);
  }

  void close() {
    if (serverTransport != NULL && serverTransport->isOpen())
      serverTransport->close();
  }

  std::shared_ptr<T> getTransport() { return serverTransport; }

  bool hasError() { return foundError; }

  std::shared_ptr<ServerConnection> getCacheKey() { return cacheKey; }

  std::shared_ptr<T> getTransporter() { return serverTransport; }

  bool operator==(const CachedTransport *rhs) const {
    return std::addressof(this) == std::addressof(rhs);
    // return *this == *rhs;
  }

  bool isOpen() { return serverTransport->isOpen(); }

  void registerService(const std::string &instance,
                       const std::string &clusterManagers) {
    // no op
  }

  void setReturnTime(uint64_t t) { lastReturnTime = t; }

  uint64_t getLastReturnTime() { return lastReturnTime; }
  void sawError(bool sawError) { foundError = true; }

 protected:
  bool foundError;
  std::string threadName;
  std::atomic<bool> reserved;

  std::string stuckThreadName;

  uint16_t ioCount;
  int16_t lastCount;
  uint64_t lastReturnTime;
  std::shared_ptr<ServerConnection> cacheKey;
  std::shared_ptr<T> serverTransport;
};
}  // namespace interconnect
#endif /* CACHEDTRANSPORT_H_ */
