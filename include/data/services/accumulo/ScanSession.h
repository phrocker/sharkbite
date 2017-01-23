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
#ifndef SCAN_SESSION_H
#define SCAN_SESSION_H

#include "../../../concurrency/Executor.h"
#include "../../constructs/Range.h"
#include "../../constructs/security/AuthInfo.h"
#include "../../constructs/security/Authorizations.h"
#include "../../constructs/column.h"

using namespace cclient::data;
using namespace cclient::data::security;


class Scan : public Executor {
protected:
    boost::shared_future<ReturnType*> *nextResult;
    std::map<std::string, std::map<std::string, std::string> > opts;
    std::vector< cclient::data::IterInfo* > iters;
    std::vector< cclient::data::Column* > columns;
    cclient::data::KeyExtent* keyExtent;
    vector<Range*> ranges;

    AuthInfo *creds;

    uin64_t scanId;
    Authorizations* auths;

public:

    Scan( uint64_t scanId) : scanId(scanId)
    {
    }

    void setFuture(boost::shared_future<ReturnType*> *future)
    {
        nextResult = future;
    }

    bool isScheduled()
    {
        return nextResult != NULL;
    }
    virtual ReturnType *run() {

    }
    void addRange ( Range *range )
    {
        ranges.push_back( range );
    }

    void setCredentials( AuthInfo *auth)
    {
        creds = auth;
    }
    void setAuthorizations ( std::vector< std::string> authorizations ) {
        auths = new Authorizations(authorizations);
    }
    void setIteratorOptions ( std::map<std::string, std::map<std::string, std::string> > ssio ) {
        opts.insert(opts.end(),ssio.begin(),ssio.end());
    }
    void setIterators ( std::vector< cclient::data::IterInfo* > iterators ) {
        iters.insert(iters.end(),iterators.begin(),iterators.end());
    }
    void setColumns ( std::vector< cclient::data::Column* > cols ) {
        columns.insert( columns.end(), columns.begin(), columns.end() );
    }
    void setExtent ( cclient::data::KeyExtent* extent ) {
        keyExtent = extent;
    }


};
#endif
