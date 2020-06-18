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

#ifndef USEROPERATIONS_H
#define USEROPERATIONS_H

#include "../../data/constructs/security/Authorizations.h"

#include "../../data/constructs/KeyValue.h"
#include "../../data/constructs/security/AuthInfo.h"
#include "../../data/constructs/security/Permissions.h"
#include "../../data/constructs/client/Instance.h"
#include "../../scanner/Source.h"
#include "../../scanner/constructs/Results.h"
#include "../transport/AccumuloCoordinatorTransporter.h"
#include "../RootInterface.h"

#include <map>
#include <string>

namespace interconnect {

/**
 * Security operations representation
 * 
 * Purpose: Provides access to all accumul operations
 * 
 * Design: Uses the thrift interface as a connector to perform the security operations.
 **/
class SecurityOperations {
 public:
  /**
   * Constructor
   * @param creds connecting credentials
   * @param instance connecting instance
   * @param interface connecting thrift interface
   * @param interconnect thrift transport pool
   **/
  SecurityOperations(cclient::data::security::AuthInfo *creds, std::shared_ptr<cclient::data::Instance> instance, std::shared_ptr<CachedTransport<interconnect::AccumuloCoordinatorTransporter>> interface,
                     TransportPool<interconnect::AccumuloCoordinatorTransporter> *distributedConnector)
      : credentials(creds),
        myInstance(instance),
        clientInterface(interface->getTransport()) {
    ptr = interface->getTransporter();
    cachedTransport = interface;
    refTransportPool = distributedConnector;
  }

  ~SecurityOperations();

  /**
   * Creates a user
   * @param user new username
   * @param password new password
   * @returns result of this security operations.
   * -1 invalid username
   * 0 user exists
   * 1 use created
   **/
  int8_t createUser(std::string user, std::string password);

  /**
   * Creates a user's password
   * @param user username
   * @param password new password
   * @returns result of this security operations.
   * -1 invalid username
   * 0 password changed
   * 1 security failure
   **/
  int8_t changeUserPassword(std::string user, std::string password);

  /**
   * Drops a user
   * @param user username
   * @returns result of this security operations.
   * -1 invalid username
   * 0 user dropped
   * 1 security failure
   **/
  int8_t dropUser(std::string user);

  /**
   * Retrieves a user's authorizations
   * @param user username
   * @returns user's authorizations object
   **/
  std::unique_ptr<cclient::data::security::Authorizations> getAuths(std::string user);

  /**
   * Grants authorizations to the user
   * @param auths auhorizations to grant to the user
   * @param user user to grant to
   * @returns result of this security operations.
   * -2 auths is null
   * -1 invalid username
   * 1 auths granted.

   **/
  int8_t grantAuthorizations(cclient::data::security::Authorizations *auths, std::string user);

  /**
   * Returns true if user has a system permission
   * @param user user to grant to
   * @param perm permission to test
   * @returns true if user has the permission, false otherwise
   **/
  bool hasSystemPermission(const std::string &user, cclient::data::SystemPermissions perm);

  /**
   * Returns true if user has a tabl;e permission
   * @param user user to grant to
   * @param table table name
   * @param perm permission to test
   * @returns true if user has the permission, false otherwise
   **/
  bool hasTablePermission(const std::string &user,const std::string &table, cclient::data::TablePermissions perm);

  /**
   * Returns true if user has a namespace permission
   * @param user user to grant to
   * @param namespace namespace name
   * @param perm permission to test
   * @returns true if user has the permission, false otherwise
   **/
  bool hasNamespacePermission(const std::string &user,const std::string &nsp, cclient::data::NamespacePermissions perm);


 /**
   * Grants permissions
   * @param user user to grant to
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t grantSystemPermission(const std::string &user, cclient::data::SystemPermissions perm);
   /**
   * Revokes permissions
   * @param user user from which to revoke permission
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t revokeSystemPermission(const std::string &user, cclient::data::SystemPermissions perm);


 /**
   * Grants permissions
   * @param user user to grant to
   * @param table table on which to grant permission
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t grantTablePermission(const std::string &user,const std::string &table, cclient::data::TablePermissions perm);
   /**
   * Revokes permissions
   * @param user user from which to revoke permission
   * @param table table on which to revoke permission
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t revokeTablePermission(const std::string &user,const std::string &table, cclient::data::TablePermissions perm);


 /**
   * Grants permissions
   * @param user user to grant to
   * @param nsp namespace on which to grant permission
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t grantNamespacePermission(const std::string &user,const std::string &nsp, cclient::data::NamespacePermissions perm);
  /**
   * Revokes permissions
   * @param user user from which to revoke permission
   * @param nsp namespace on which to revoke permission
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t revokeNamespacePermission(const std::string &user,const std::string &nsp, cclient::data::NamespacePermissions perm);
 protected:
  // shared pointer for the thrift transport
  std::shared_ptr<interconnect::AccumuloCoordinatorTransporter> ptr;
  // client interface
  std::shared_ptr<interconnect::AccumuloCoordinatorTransporter> clientInterface;
  // cached transport that can be replaced in light of failure.
  std::shared_ptr<CachedTransport<interconnect::AccumuloCoordinatorTransporter>> cachedTransport;
  // distributed connector
  TransportPool<interconnect::AccumuloCoordinatorTransporter> *refTransportPool;
  std::shared_ptr<cclient::data::Instance> myInstance;
  cclient::data::security::AuthInfo *credentials;

};
}
#endif // USEROPERATIONS_H
