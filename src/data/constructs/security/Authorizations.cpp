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

#include "data/constructs/security/Authorizations.h"


#include <string>
#include <set>



namespace cclient{
  namespace data{
    namespace security{

Authorizations::Authorizations ()
{

}

Authorizations::Authorizations (std::string authorizations, char *validCharacters,
                                int valid)
{
    for (int i = 0; i < valid; i++)
    {
        validAuthChars[(uint8_t) validCharacters[i]] = true;
    }
    addAuthorization (authorizations);
}


Authorizations::Authorizations (std::vector<std::string> *authorizations)
{
    for (auto it = authorizations->begin (); it != authorizations->end (); it++)
        addAuthorization (*it);

}

void
Authorizations::addAuthorization (std::string auth)
{
    authStrings.insert (auth);
}

Authorizations::~Authorizations ()
{

}
    }
  }
}