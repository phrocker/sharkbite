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
#include <algorithm>
#include <set>

#include <vector>

#include "../../exceptions/ClientException.h"

namespace cclient {
namespace data {
namespace security {

class AuthsInit {
 public:
  static AuthsInit* init() {
    static AuthsInit initer;
    return &initer;
  }

  bool* getDefaultAuths() {
    return &validAuthChars[0];
  }
 private:
  AuthsInit() {
    buildDefaultAuths();
  }

  bool validAuthChars[256];
  int buildDefaultAuths();
};

class Authorizations {
 public:
  Authorizations();

  Authorizations(std::string authorizations, char *validCharacters, int valid);

  ~Authorizations();

  explicit Authorizations(std::vector<std::string>*);
  explicit Authorizations(const std::vector<std::string>&);

  void addAuthorization(std::string auth);

  std::vector<std::string> getAuthorizations() const {
    std::vector<std::string> strAuths;
    for (auto it : authStrings) {
      strAuths.push_back(it);
    }
    return strAuths;
  }

  bool empty() const {
    return authStrings.empty();
  }

  bool contains(const std::string &viz) const {
    return std::find(authStrings.begin(), authStrings.end(), viz) != authStrings.end();
  }

  bool operator ==(const Authorizations &rhs) const {
    return authStrings == rhs.authStrings;
  }

  static bool isValidAuthCharacter(char c);

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

  std::set<std::string> authStrings;

};

}
}
}
#endif /* AUTHORIZATIONS_H_ */
