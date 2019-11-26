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

#ifndef CLIENTINTERFACE_H_
#define CLIENTINTERFACE_H_

#include <type_traits>
#include <string>

#include "transport/Transport.h"

#include "../data/constructs/security/AuthInfo.h"

namespace interconnect {

/**
 * ClientInterface is the lowest level connecting transport mechanism
 * for clients.
 **/
template<typename Tr>
class ClientInterface {
 public:
  /**
   * Constructor that accepts host and port. This does not
   * support a connection as a transport must first be given to this instance
   * @param host host name we're connecting to.
   * @param port port for the aforementioned host name.
   **/
  explicit ClientInterface(const std::string &host, const int port);

  ClientInterface()
      : server_port(9997),
        authenticated(false) {
  }

  virtual ~ClientInterface();

  /**
   * Pure virtual function that supports authenticating users
   * against this client connection.
   * @param username username
   * @param password username's password.
   **/
  virtual void authenticate(const std::string &username, const std::string &password) = 0;

  /**
   * A coupling mechanism that allows us to use cclient::data::security::AuthInfo vice the username
   * and password authentication above.
   * @param auth authorization info that contains the username and password.
   **/
  void authenticate(const cclient::data::security::AuthInfo * const auth) {
    authenticate(auth->getUserName(), auth->getPassword());
  }

  /**
   * Sets the underlying transport within this client connection.
   * @param transporty transporter, based on T.
   **/
  void setTransport(const std::shared_ptr<Tr> &transporty) {

    transport = transporty;
    transport->registerService(instanceId, zookeepers);
  }

  /**
   * Deprecated function to ensure authentication is only performed once.
   * @param auth whether or not the user is authenticated.
   **/
  bool setAuthenticated(bool auth) {
    authenticated = auth;
    return auth;
  }

  /**
   * Set the user credentials without authenticating.
   * @param user username
   * @param password password for user.
   **/
  void setCredentials(const std::string &user, const std::string &password) {
    authenticated_user = user;
    authenticated_password = password;

  }

  /**
   * Gets the transport used within this client connection.
   * @returns transport shared pointer.
   **/
  std::shared_ptr<Tr> getTransport() {
    return transport;
  }

 protected:
  std::shared_ptr<Tr> transport;

  std::string server_host;
  int server_port;
  bool authenticated;

  std::string authenticated_user;
  std::string authenticated_password;
  // info aboutt cluster
  std::string instanceId;
  std::string zookeepers;

};

template<typename Tr>
ClientInterface<Tr>::ClientInterface(const std::string &host, const int port)
    : authenticated(false),
      server_host(host),
      server_port(port) {

}

template<typename Tr>
ClientInterface<Tr>::~ClientInterface() {
}

}
#endif /* CLIENTINTERFACE_H_ */
