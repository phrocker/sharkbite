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
#include "data/constructs/server/ServerDefinition.h"

#include <string>



#include "data/constructs/server/../security/AuthInfo.h"
#include "data/constructs/server/../security/Authorizations.h"
#include "data/constructs/server/../Range.h"
#include "data/constructs/server/../KeyExtent.h"

namespace cclient
{

namespace data
{

namespace tserver
{

/**
 * Constructor
 * @param creds ptr to creds
 * @param auths ptr to auths
 * @param host host name we're connecting to
 * @param port incoming port
 */
ServerDefinition::ServerDefinition (cclient::data::security::AuthInfo *creds,
                                    cclient::data::security::Authorizations *auths, std::string host,
                                    uint32_t port) :
    credentials (creds), auths (auths), server (host), port (port)
{

}
// default to a port

ServerDefinition::~ServerDefinition ()
{

}

cclient::data::security::AuthInfo *
ServerDefinition::getCredentials ()
{
    return credentials;
}

cclient::data::security::Authorizations *
ServerDefinition::getAuthorizations () const
{
    return auths;
}

std::string
ServerDefinition::getServer () const
{
    return server;
}

uint32_t
ServerDefinition::getPort () const
{
    return port;
}
}
}
}
