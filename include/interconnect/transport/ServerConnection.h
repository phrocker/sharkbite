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

#ifndef SERVERCONNECTION_H_
#define SERVERCONNECTION_H_

#include <iostream>
#include <memory>
#include "data/constructs/inputvalidation.h"
#include "data/exceptions/IllegalArgumentException.h"

namespace interconnect {

class ServerConnection {
 public:

  explicit ServerConnection(const std::shared_ptr<ServerConnection> &other)
      : host(other->host),
        port(other->port),
        timeout(other->timeout) {
  }
  ServerConnection(std::string loc, uint16_t port, uint64_t timeout)
      : host(loc),
        port(port),
        timeout(timeout) {
    if (IsEmpty(&loc)) {
      throw cclient::exceptions::IllegalArgumentException("Invalid Input; host name is empty");
    }

  }

  virtual ~ServerConnection() {

  }

  std::string getHost() const {
    return host;
  }

  uint16_t getPort() const {
    return port;
  }

  uint64_t getTimeout() const {
    return timeout;
  }

    std::ostream& operator<<(std::ostream& os) {
        return os << host << ":" << port << " " << timeout << std::endl;
    }


    ServerConnection &operator=(const ServerConnection &rhs) {

    host = rhs.host;
    port = rhs.port;
    timeout = rhs.timeout;
    return *this;
  }

  bool operator==(const ServerConnection &rhs) {
    bool result = true;
    result = (host == rhs.host);
    if (!result)
      return false;
    result = (port == rhs.port);
    if (!result)
      return false;
    result = (timeout == rhs.timeout);
    if (!result)
      return false;
    else
      return true;
  }

  bool operator!=(const ServerConnection &rhs) {
    return !(*this == rhs);
  }

  bool operator<(const ServerConnection &rhs) const {
    bool result = true;
    result = (host < rhs.host);
    if (!result)
      return false;
    result = (port < rhs.port);
    if (!result)
      return false;
    result = (timeout < rhs.timeout);
    if (!result)
      return false;
    else
      return true;
  }

    bool operator<(const std::shared_ptr<ServerConnection> &rhs) const {
        bool result = true;
        result = (host < rhs->host);
        if (!result)
            return false;
        result = (port < rhs->port);
        if (!result)
            return false;
        result = (timeout < rhs->timeout);
        if (!result)
            return false;
        else
            return true;
    }

  bool operator>(const ServerConnection &rhs) const {
    bool result = true;
    result = (host > rhs.host);
    if (!result)
      return false;
    result = (port > rhs.port);
    if (!result)
      return false;
    result = (timeout > rhs.timeout);
    if (!result)
      return false;
    else
      return true;
  }

 protected:
  std::string host;
  uint16_t port;
  uint64_t timeout;
};

struct Cmp_ServerConnection {
  bool operator()(const ServerConnection& first, const ServerConnection& second) {
    bool less = first.getHost() < second.getHost();
    if (less)
      return true;
    else {
      if (first.getHost() > second.getHost())
        return false;
      else {
        less = first.getPort() < second.getPort();
        if (less)
          return true;
        else {
          if (first.getPort() > second.getPort())
            return false;
          else {
            return first.getTimeout() < second.getTimeout();
          }
        }
      }
    }
  }
};

struct Cmp_ServerConnectionSP {
    bool operator()(const std::shared_ptr<ServerConnection>& first, const  std::shared_ptr<ServerConnection>& second) {
        return *first.get() < *second.get();
    }
};


}
#endif /* SERVERCONNECTION_H_ */
