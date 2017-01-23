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

#include "../../../include/interconnect/securityOps/SecurityOperations.h"
#include "../../../include/data/exceptions/ClientException.h"
namespace interconnect{


SecurityOperations::~SecurityOperations()
{
  refTransportPool->freeTransport(cachedTransport);
  clientInterface->close();
}

int8_t SecurityOperations::dropUser(std::string user)
{
  if (IsEmpty(&user))
  {
    return -1;
  }
 // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  if ( clientInterface->dropUser(credentials,user) )
    return 0;
  else return 1;
}

int8_t SecurityOperations::changeUserPassword(std::string user, std::string password)
{
  if (IsEmpty(&user))
  {
    return -1;
  }
  clientInterface->closeAndCreateClient();
  if ( clientInterface->changeUserPassword(credentials,user,password) )
    return 0;
  else return 1;
}


int8_t 
SecurityOperations::createUser(std::string user, std::string password)
{
  if (IsEmpty(&user))
  {
    return -1;
  }
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  if ( clientInterface->createUser(credentials,user,password))
    return 1;
  else
    return 0;
}

      
std::unique_ptr<cclient::data::security::Authorizations> SecurityOperations::getAuths(std::string user)
{
  if (IsEmpty(&user))
    throw cclient::exceptions::ClientException(ARGUMENT_CANNOT_BE_NULL);
   // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  return std::unique_ptr<cclient::data::security::Authorizations>(clientInterface->getUserAuths(credentials,user));
}

int8_t SecurityOperations::grantAuthorizations(cclient::data::security::Authorizations* auths, std::string user)
{
  if (IsEmpty(auths))
    return -2;
  if (IsEmpty(&user))
    return -1;
  // create the client interface so that this service is usable.
  clientInterface->closeAndCreateClient();
  clientInterface->changeUserAuths(credentials,user,auths);
  return 1;
}

}