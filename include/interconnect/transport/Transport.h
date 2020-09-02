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
#ifndef TRANSPORT_H_
#define TRANSPORT_H_

#include "ServerConnection.h"

namespace interconnect {

template <typename T>
class Transporter {
 protected:
  virtual void newTransporter(
      const std::shared_ptr<ServerConnection> &conn) = 0;

 public:
  explicit Transporter(const std::shared_ptr<ServerConnection> &conn) {}

  virtual ~Transporter() {}

  virtual T getTransport() = 0;

  virtual void registerService(const std::string &instance,
                               const std::string &clusterManagers) = 0;
  virtual bool open() = 0;
  virtual bool isOpen() = 0;
  virtual void close() = 0;
};

}  // namespace interconnect

#endif /* TRANSPORT_H_ */
