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


#ifndef AUTHORIZATIONS_H_
#define AUTHORIZATIONS_H_ 1

#pragma once


#include <string>
#include <set>

#include <vector>



#include "../../exceptions/ClientException.h"


namespace cclient {
namespace data {
namespace security {

static bool validAuthChars[256];

class Authorizations {
public:
    Authorizations();

    Authorizations(std::string authorizations, char *validCharacters, int valid);

    ~Authorizations();

    explicit Authorizations(std::vector<std::string> *);
    
    void addAuthorization(std::string auth);

    std::vector<std::string> getAuthorizations() {
        std::vector<std::string> strAuths;
        for (auto it : authStrings) {
            strAuths.push_back(it);
        }
        return strAuths;
    }

    inline static int init_auths() {
        return buildDefaultAuths();
    }
    
    
    bool
    operator == (const Authorizations & rhs) const
    {
      return authStrings == rhs.authStrings;
    }

protected:

    void validateAuths() {
        for (auto it = authStrings.begin(); it != authStrings.end(); it++) {
            for (uint32_t i = 0; i < (*it).size(); i++) {
                if (!isValidAuthCharacter((*it).at(i))) {
                    throw cclient::exceptions::ClientException("Invalid authorization character");
                }
            }
        }
    }
    static int buildDefaultAuths() {
        for (uint8_t i = 0; i < 256; i++) {
            validAuthChars[i] = false;
        }

        for (uint8_t i = 'a'; i <= 'z'; i++) {
            validAuthChars[i] = true;
        }

        for (uint8_t i = 'A'; i <= 'Z'; i++) {
            validAuthChars[i] = true;
        }

        for (uint8_t i = '0'; i <= '9'; i++) {
            validAuthChars[i] = true;
        }

        validAuthChars['_'] = true;
        validAuthChars['-'] = true;
        validAuthChars[':'] = true;
        return 0;
    }

    static bool isValidAuthCharacter(char c) {
        return validAuthChars[(uint8_t)c];
    }

    std::set<std::string> authStrings;

};

}
}
}
#endif /* AUTHORIZATIONS_H_ */
