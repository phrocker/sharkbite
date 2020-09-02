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

#include "interconnect/securityOps/SecurityOperations.h"

#include "data/exceptions/ClientException.h"
namespace interconnect {

SecurityOperations::~SecurityOperations() {
  refTransportPool->freeTransport(cachedTransport);
  clientInterface->close();
}

int8_t SecurityOperations::dropUser(std::string user) {
  if (IsEmpty(&user)) {
    return -1;
  }
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  if (clientInterface->dropUser(credentials, user))
    return 0;
  else
    return 1;
}

int8_t SecurityOperations::changeUserPassword(std::string user,
                                              std::string password) {
  if (IsEmpty(&user)) {
    return -1;
  }
  clientInterface->closeAndCreateClient();
  if (clientInterface->changeUserPassword(credentials, user, password))
    return 0;
  else
    return 1;
}

int8_t SecurityOperations::createUser(std::string user, std::string password) {
  if (IsEmpty(&user)) {
    return -1;
  }
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  if (clientInterface->createUser(credentials, user, password))
    return 1;
  else
    return 0;
}

std::unique_ptr<cclient::data::security::Authorizations>
SecurityOperations::getAuths(std::string user) {
  if (IsEmpty(&user))
    throw cclient::exceptions::ClientException(ARGUMENT_CANNOT_BE_NULL);
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  return std::unique_ptr<cclient::data::security::Authorizations>(
      clientInterface->getUserAuths(credentials, user));
}

int8_t SecurityOperations::grantAuthorizations(
    cclient::data::security::Authorizations *auths, std::string user) {
  if (IsEmpty(auths)) return -2;
  if (IsEmpty(&user)) return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  clientInterface->changeUserAuths(credentials, user, auths);
  return 1;
}

int8_t SecurityOperations::grantSystemPermission(
    const std::string &user, cclient::data::SystemPermissions perm) {
  if (IsEmpty(&user)) return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  clientInterface->grant(credentials, user, perm);
  return 1;
}

int8_t SecurityOperations::revokeSystemPermission(
    const std::string &user, cclient::data::SystemPermissions perm) {
  if (IsEmpty(&user)) return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  clientInterface->revoke(credentials, user, perm);
  return 1;
}

int8_t SecurityOperations::grantTablePermission(
    const std::string &user, const std::string &table,
    cclient::data::TablePermissions perm) {
  if (IsEmpty(&user)) return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  clientInterface->grant(credentials, user, table, perm);
  return 1;
}

int8_t SecurityOperations::revokeTablePermission(
    const std::string &user, const std::string &table,
    cclient::data::TablePermissions perm) {
  if (IsEmpty(&user)) return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  clientInterface->revoke(credentials, user, table, perm);
  return 1;
}

int8_t SecurityOperations::grantNamespacePermission(
    const std::string &user, const std::string &nsp,
    cclient::data::NamespacePermissions perm) {
  if (IsEmpty(&user)) return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  clientInterface->grant(credentials, user, nsp, perm);
  return 1;
}

int8_t SecurityOperations::revokeNamespacePermission(
    const std::string &user, const std::string &nsp,
    cclient::data::NamespacePermissions perm) {
  if (IsEmpty(&user)) return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  clientInterface->revoke(credentials, user, nsp, perm);
  return 1;
}

bool SecurityOperations::hasSystemPermission(
    const std::string &user, cclient::data::SystemPermissions perm) {
  if (IsEmpty(&user)) return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  return clientInterface->hasPermission(credentials, user, perm);
}

bool SecurityOperations::hasTablePermission(
    const std::string &user, const std::string &table,
    cclient::data::TablePermissions perm) {
  if (IsEmpty(&user)) return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  return clientInterface->hasPermission(credentials, user, table, perm);
}

bool SecurityOperations::hasNamespacePermission(
    const std::string &user, const std::string &nsp,
    cclient::data::NamespacePermissions perm) {
  if (IsEmpty(&user)) return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  return clientInterface->hasPermission(credentials, user, nsp, perm);
}

}  // namespace interconnect