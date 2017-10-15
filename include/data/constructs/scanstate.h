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

#ifndef SCANSTATE_H
#define SCANSTATE_H


#include <map>

#include <vector>


#include "inputvalidation.h"
#include "Range.h"
#include "security/AuthInfo.h"
#include "security/Authorizations.h"
#include "../client/TabletLocation.h"
#include "column.h"
#include "IterInfo.h"
#include "../extern/thrift/data_types.h"
#include "../exceptions/ClientException.h"

namespace cclient {
namespace data {


class ScanState {

public:
    ScanState() {

    }
    ScanState(cclient::data::security::AuthInfo *credentials, std::string tablename, cclient::data::security::Authorizations *auths,
              Range *range, std::set<Column*> *fetchedColumns, uint64_t size,
              std::vector<cclient::data::IterInfo*> *iters,
              std::map<std::string, std::map<std::string, std::string>> *options, bool isolated) :
        isolated(isolated), tableName(tablename), credentials(credentials), auths(
            auths), skipStartRow(false), size(size), scanId(0) {
        if (IsEmpty(credentials) || IsEmpty(&tablename) || IsEmpty(auths)) {
            throw cclient::exceptions::ClientException(
                "Credentials, tablename, and authorizations cannot be null");
        }

        if (!IsEmpty(fetchedColumns))
            columns.insert(columns.begin(), fetchedColumns->begin(),
                           fetchedColumns->end());

        if (!IsEmpty(iters))
            serverSideIteratorList.insert(serverSideIteratorList.begin(),
                                          iters->begin(), iters->end());

        if (!IsEmpty(options))
            serverSideOptions.insert(options->begin(), options->end());

        auto startKey = range->getStartKey();

        if (nullptr == startKey) {
            startKey = std::make_shared<Key>();
        }

        std::pair<char*, size_t> row = startKey->getRow();
        startRow = std::string(row.first, row.second);

    }
    ScanState(const ScanState& other);
    virtual ~ScanState();
    virtual ScanState& operator=(const ScanState& other);
    virtual bool operator==(const ScanState& other) const;

    bool isIsolated() const {
        return isolated;
    }

    std::string getTableName() const {
        return tableName;
    }

    std::string getStartRow() const {
        return startRow;
    }

    Range *getRange() const {
        return myRange;
    }

    uint64_t getSize() const {
        return size;
    }

    cclient::data::security::AuthInfo *getCredentials() const {
        return credentials;
    }

    cclient::data::security::Authorizations *getAuths() const {
        return auths;
    }

    TabletLocation *getPrevious() const {
        return prevLoc;
    }

    uint64_t getScanId() const {
        return scanId;
    }

    bool isFinished() {
        return finished;
    }

    bool setFinished(bool fin = true) {
        finished = fin;
	return fin;
    }

    const std::vector<cclient::data::IterInfo*> *getIteratorList() const {
        return &serverSideIteratorList;
    }

    const std::map<std::string, std::map<std::string, std::string> > *getOptions() const {
        return &serverSideOptions;
    }

protected:
    bool isolated;
    std::string tableName;
    std::string startRow;
    bool skipStartRow;
    Range *myRange;

    uint64_t size;

    cclient::data::security::AuthInfo *credentials;

    cclient::data::security::Authorizations *auths;

    std::vector<Column*> columns;

    TabletLocation *prevLoc;

    uint64_t scanId;

    bool finished;

    std::vector<cclient::data::IterInfo*> serverSideIteratorList;

    std::map<std::string, std::map<std::string, std::string> > serverSideOptions;

};

}
}
#endif // SCANSTATE_H
