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

#ifndef SERVERTRANSPORT_H_
#define SERVERTRANSPORT_H_

#include "Transport.h"

#include "../scanrequest/ScanRequest.h"
#include "../Scan.h"
#include "../../data/constructs/KeyExtent.h"
#include "../../data/constructs/Range.h"

namespace interconnect {

template<class T, class K, class V, class W>
class ServerTransport: public Transporter<T> {
public:

    explicit ServerTransport(ServerConnection *conn) :
        Transporter<T>(conn) {

    }
    virtual ~ServerTransport() {

    }

    virtual void authenticate(cclient::data::security::AuthInfo *auth) = 0;

    virtual Scan *beginScan(ScanRequest<ScanIdentifier<K, V> > *req) = 0;

    virtual void *write(cclient::data::security::AuthInfo *auth, std::map<K, std::vector<W>> *request) = 0;
};

} /* namespace interconnect */
#endif /* SERVERTRANSPORT_H_ */
