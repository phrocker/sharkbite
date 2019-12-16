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

#ifndef SCANREQUEST_H_
#define SCANREQUEST_H_

#include <vector>

#include "../../data/constructs/IterInfo.h"
#include "../../data/constructs/column.h"

#include "../../data/constructs/security/AuthInfo.h"
#include "../../data/constructs/security/Authorizations.h"
#include "../../data/constructs/inputvalidation.h"
#include "../../data/exceptions/IllegalArgumentException.h"
#include "../transport/ServerConnection.h"
#include "ScanIdentifier.h"

namespace interconnect {
/**
 * Represents a scan request
 */

template<typename I>
class ScanRequest {

 public:
  ScanRequest(cclient::data::security::AuthInfo *credentials, cclient::data::security::Authorizations *auths, std::shared_ptr<ServerConnection> server)
      :
      creds(credentials),
      auths(auths),
      connection(server), singlebufferSize(1000) {

    if (IsEmpty(credentials) || IsEmpty(auths) || IsEmpty(server.get())) {
      throw cclient::exceptions::IllegalArgumentException("credentials, authorizations, and server must not be empty");
    }

  }

  virtual ~ScanRequest() {
    for (auto it : identifiers) {
      delete it;
    }
  }

  void setBufferSize(uint32_t size){
    singlebufferSize = size;
  }

  virtual void setIters(const std::vector<cclient::data::IterInfo> &iters) {
    iterators.insert(iterators.end(), iters.begin(), iters.end());
  }

  void addColumn(cclient::data::Column *col) {
    columns.push_back(*col);
  }

  void addColumn(const cclient::data::Column &col) {
    columns.push_back(col);
  }

  void addColumns(const std::vector<cclient::data::Column> &cols) {
    columns.insert(columns.end(), cols.begin(), cols.end());
  }

  std::vector<cclient::data::Column> getColumns() const {
    return columns;
  }

  cclient::data::security::AuthInfo* getCredentials() const {
    return creds;
  }

  cclient::data::security::Authorizations* getAuthorizations() const {
    return auths;
  }

  uint32_t getBuffersize(){
    return singlebufferSize;
  }

  std::vector<cclient::data::IterInfo> getIterators() {
    return iterators;
  }

  void putIdentifier(I *ident) {
    identifiers.push_back(ident);
  }

  std::vector<I*>* getRangeIdentifiers() {
    return &identifiers;
  }

 protected:

  uint32_t singlebufferSize;

  std::vector<I*> identifiers;
  cclient::data::security::AuthInfo *creds;
  cclient::data::security::Authorizations *auths;
  std::vector<cclient::data::IterInfo> iterators;
  std::vector<cclient::data::Column> columns;
  std::shared_ptr<ServerConnection> connection;
};

} /* namespace interconnect */
#endif /* SCANREQUEST_H_ */
