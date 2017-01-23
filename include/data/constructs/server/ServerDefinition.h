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

#ifndef SERVERDEFINITION_H_
#define SERVERDEFINITION_H_


#include <string>



#include "../security/AuthInfo.h"
#include "../security/Authorizations.h"
#include "../Range.h"
#include "../KeyExtent.h"

namespace cclient {

namespace data {
namespace tserver {

/**
 * Defines the definition of a server connection. 
 * 
 * Design: Contains the authorization info, and server/port for a server.
 * 
 **/
class ServerDefinition {
public:
    /**
     * Constructor
     * @param creds ptr to creds
     * @param auths ptr to auths
     * @param host host name we're connecting to
     * @param port incoming port
     */
    ServerDefinition(cclient::data::security::AuthInfo *creds, cclient::data::security::Authorizations *auths, std::string host,
                     uint32_t port);

    // default to a port

    ~ServerDefinition();

    /**
     * Returns a pointer the credentials for this server connect.
     * @returns auth info object.
     **/
    cclient::data::security::AuthInfo *getCredentials();

    /**
     * Returns a pointer the credentials for this server connect.
     * @returns authorizations for this connection
     **/
    cclient::data::security::Authorizations *getAuthorizations() const;

    /**
     * Returns the server host name
     * @returns host name
     **/
    std::string getServer() const;

    /**
     * Returns the port for the connecting server definition
     * @returns port
     **/
    uint32_t getPort() const;

protected:
   // connecting credentials
    cclient::data::security::AuthInfo *credentials;
    // authorizations
    cclient::data::security::Authorizations *auths;
    // server host name
    std::string server;
    // port 
    uint32_t port;

};
}
}
}
#endif /* SERVERDEFINITION_H_ */
