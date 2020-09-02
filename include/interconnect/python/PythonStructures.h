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
#pragma once

#include <memory>

#include <interconnect/namespaceOps/NamespaceOperations.h>
#include <interconnect/securityOps/SecurityOperations.h>
#include <interconnect/tableOps/ClientTableOps.h>

namespace interconnect {

class PythonTableOperations {
 public:
  explicit PythonTableOperations(
      const std::shared_ptr<AccumuloTableOperations> &tops)
      : table_opts(tops) {}

  /**
   * Create a table.
   * @param recreate will recreate a table if necessary
   * @return whether or not the table was created.
   **/
  bool create(bool recreate = false) { return table_opts->create(recreate); }

  /**
   * Removes the current table
   * @return whether or not removal was successful.
   **/
  bool remove() { return table_opts->remove(); }

  /**
   * Determines if the table exists.
   * @param createIfNot will create the table if true and it does not exist
   * @return returns boolean of whether or not the table exists
   **/
  bool exists(bool createIfNot = false) {
    return table_opts->exists(createIfNot);
  }

  /**
   * Imports rfiles in path specified in dir
   * @param dir dir containing rfiles
   * @param fail_path failure path directory
   * @param setTime Accumulo will set the time
   * @return status of create
   **/
  bool import(std::string dir, std::string fail_path, bool setTime = false) {
    return table_opts->import(dir, fail_path, setTime);
  }

  /**
   * Flushes the current table
   * @param startRow start row on which to begin the flush
   * @param endRow end point for the flush
   * @param wait wait on this operation before returning
   * @return status of flush
   * 0 failure
   * 1 success, flush occurred
   **/
  int8_t flush(std::string startRow, std::string endRow, bool wait) {
    return table_opts->flush(startRow, endRow, wait);
  }

  /**
   * Compacts the current table
   * @param startRow start row on which to begin the compaction
   * @param endRow end point for the compaction
   * @param wait wait on this operation before returning
   * @return status of compaction
   * 0 failure
   * 1 success, compaction occurred
   **/
  int8_t compact(std::string startRow, std::string endRow, bool wait) {
    return table_opts->compact(startRow, endRow, wait);
  }

  /**
   * Sets a table property
   * @param property property names
   * @param value property value
   * -1 property is empty
   * 0 successs
   */
  int8_t setProperty(std::string property, std::string value) {
    return table_opts->setProperty(property, value);
  }

  /**
   * Removes a property on this table.
   * @param property property name to remove
   * -1 property is empty
   * success
   */
  int8_t removeProperty(std::string property) {
    return table_opts->removeProperty(property);
  }

  /**
   * Adds splits for the current table
   * @param partitions table splits
   **/
  void addSplits(std::set<std::string> partitions) {
    table_opts->addSplits(partitions);
  }

  /**
   * Add a new constraint to the current table.
   * @param constraintClassName full canonical name of the constraint class
   * @return unique number assigned to the constraint
   **/
  int addConstraint(std::string constraintClassName) {
    return table_opts->addConstraint(constraintClassName);
  }

  /**
   * Creates a new scanner
   * @param auths authorizations for this scanner
   * @param threads current threads
   * @return new scanner
   **/
  auto createScanner(cclient::data::security::Authorizations *auths,
                     uint16_t threads) {
    return table_opts->createSharedScanner(auths, threads);
  }

  auto createWriter(cclient::data::security::Authorizations *auths,
                    uint16_t threads) {
    return table_opts->createSharedWriter(auths, threads);
  }

 private:
  std::shared_ptr<AccumuloTableOperations> table_opts;
};

class PythonNamespaceOperations {
 public:
  explicit PythonNamespaceOperations(
      const std::shared_ptr<NamespaceOperations> &tops)
      : namespace_ops(tops) {}

  /**
   * Provides a list of namespaces
   * @return namespaces
   **/
  std::vector<std::string> list() { return namespace_ops->list(); }
  /**
   * Removes a namespace if specified, otherwise myNamespace is removed
   * @param name namespace to remove (optional )
   * @return status of operation.
   **/
  bool remove(std::string name = "") { return namespace_ops->remove(name); }

  /**
   * Determines if namespace exists
   * @param name namespace to test (optional )
   * @return true if namespace exists, false otherwise
   **/
  bool exists(std::string name = "") { return namespace_ops->exists(name); }

  /**
   * Creates a namespace
   * @param name namespace to create (optional, otherwise myNamespace )
   **/
  void create(std::string name = "") { return namespace_ops->create(name); }

  /**
   * Renames a namespace
   * @param newName new namespace name
   * @param oldName previous name. if not specified myNamespace will be renamed
   **/
  void rename(std::string newName, std::string oldName = "") {
    return namespace_ops->rename(newName, oldName);
  }

  /**
   * Sets a namespace property
   * @param property property names
   * @param value property value
   */
  void setProperty(std::string property, std::string value,
                   std::string nm = "") {
    namespace_ops->setProperty(property, value, nm);
  }

  /**
   * Removes a property on this namespace.
   * @param property property name to remove
   */
  void removeProperty(std::string property, std::string nm = "") {
    namespace_ops->removeProperty(property, nm);
  }

 private:
  std::shared_ptr<NamespaceOperations> namespace_ops;
};

class PythonSecurityOperations {
 public:
  explicit PythonSecurityOperations(
      const std::shared_ptr<SecurityOperations> &tops)
      : security_ops(tops) {}

  /**
   * Creates a user
   * @param user new username
   * @param password new password
   * @returns result of this security operations.
   * -1 invalid username
   * 0 user exists
   * 1 use created
   **/
  int8_t createUser(std::string user, std::string password) {
    return security_ops->createUser(user, password);
  }

  /**
   * Creates a user's password
   * @param user username
   * @param password new password
   * @returns result of this security operations.
   * -1 invalid username
   * 0 password changed
   * 1 security failure
   **/
  int8_t changeUserPassword(std::string user, std::string password) {
    return security_ops->changeUserPassword(user, password);
  }

  /**
   * Drops a user
   * @param user username
   * @returns result of this security operations.
   * -1 invalid username
   * 0 user dropped
   * 1 security failure
   **/
  int8_t dropUser(std::string user) { return security_ops->dropUser(user); }

  /**
   * Retrieves a user's authorizations
   * @param user username
   * @returns user's authorizations object
   **/
  std::unique_ptr<cclient::data::security::Authorizations> getAuths(
      std::string user) {
    return security_ops->getAuths(user);
  }

  /**
   * Grants authorizations to the user
   * @param auths auhorizations to grant to the user
   * @param user user to grant to
   * @returns result of this security operations.
   * -2 auths is null
   * -1 invalid username
   * 1 auths granted.

   **/
  int8_t grantAuthorizations(cclient::data::security::Authorizations *auths,
                             std::string user) {
    return security_ops->grantAuthorizations(auths, user);
  }

  /**
   * Returns true if user has a system permission
   * @param user user to grant to
   * @param perm permission to test
   * @returns true if user has the permission, false otherwise
   **/
  bool hasSystemPermission(const std::string &user,
                           cclient::data::SystemPermissions perm) {
    return security_ops->hasSystemPermission(user, perm);
  }

  /**
   * Returns true if user has a tabl;e permission
   * @param user user to grant to
   * @param table table name
   * @param perm permission to test
   * @returns true if user has the permission, false otherwise
   **/
  bool hasTablePermission(const std::string &user, const std::string &table,
                          cclient::data::TablePermissions perm) {
    return security_ops->hasTablePermission(user, table, perm);
  }

  /**
   * Returns true if user has a namespace permission
   * @param user user to grant to
   * @param namespace namespace name
   * @param perm permission to test
   * @returns true if user has the permission, false otherwise
   **/
  bool hasNamespacePermission(const std::string &user, const std::string &nsp,
                              cclient::data::NamespacePermissions perm) {
    return security_ops->hasNamespacePermission(user, nsp, perm);
  }

  /**
   * Grants permissions
   * @param user user to grant to
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t grantSystemPermission(const std::string &user,
                               cclient::data::SystemPermissions perm) {
    return security_ops->grantSystemPermission(user, perm);
  }
  /**
   * Revokes permissions
   * @param user user from which to revoke permission
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t revokeSystemPermission(const std::string &user,
                                cclient::data::SystemPermissions perm) {
    return security_ops->revokeSystemPermission(user, perm);
  }

  /**
   * Grants permissions
   * @param user user to grant to
   * @param table table on which to grant permission
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t grantTablePermission(const std::string &user, const std::string &table,
                              cclient::data::TablePermissions perm) {
    return security_ops->grantTablePermission(user, table, perm);
  }
  /**
   * Revokes permissions
   * @param user user from which to revoke permission
   * @param table table on which to revoke permission
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t revokeTablePermission(const std::string &user,
                               const std::string &table,
                               cclient::data::TablePermissions perm) {
    return security_ops->revokeTablePermission(user, table, perm);
  }

  /**
   * Grants permissions
   * @param user user to grant to
   * @param nsp namespace on which to grant permission
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t grantNamespacePermission(const std::string &user,
                                  const std::string &nsp,
                                  cclient::data::NamespacePermissions perm) {
    return security_ops->grantNamespacePermission(user, nsp, perm);
  }
  /**
   * Revokes permissions
   * @param user user from which to revoke permission
   * @param nsp namespace on which to revoke permission
   * @param perm permission to grant
   * @returns result of this security operations.
   * -1 grant failed
   * 1 grant succeeded.
   **/
  int8_t revokeNamespacePermission(const std::string &user,
                                   const std::string &nsp,
                                   cclient::data::NamespacePermissions perm) {
    return security_ops->revokeNamespacePermission(user, nsp, perm);
  }

 private:
  std::shared_ptr<SecurityOperations> security_ops;
};

}  // namespace interconnect