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

#include <iostream>
#include <set>
#include <string>

namespace cclient {
namespace data {
namespace security {

int AuthsInit::buildDefaultAuths() {
  for (int i = 0; i < 256; i++) {
    validAuthChars[i] = false;
  }

  for (int i = 'a'; i <= 'z'; i++) {
    validAuthChars[i] = true;
  }

  for (int i = 'A'; i <= 'Z'; i++) {
    validAuthChars[i] = true;
  }

  for (int i = '0'; i <= '9'; i++) {
    validAuthChars[i] = true;
  }

  validAuthChars['_'] = true;
  validAuthChars['-'] = true;
  validAuthChars[':'] = true;
  return 0;
}

Authorizations::Authorizations() { AuthsInit::init(); }

bool Authorizations::isValidAuthCharacter(char c) {
  return AuthsInit::init()->getDefaultAuths()[(int)c];
}

Authorizations::Authorizations(std::string authorizations,
                               char *validCharacters, int valid) {
  AuthsInit::init();
  for (int i = 0; i < valid; i++) {
    AuthsInit::init()->getDefaultAuths()[(uint8_t)validCharacters[i]] = true;
  }
  addAuthorization(authorizations);
}

Authorizations::Authorizations(std::vector<std::string> *authorizations) {
  AuthsInit::init();
  for (auto it = authorizations->begin(); it != authorizations->end(); it++)
    addAuthorization(*it);
}

Authorizations::Authorizations(const std::vector<std::string> &authorizations) {
  AuthsInit::init();
  for (auto it = authorizations.begin(); it != authorizations.end(); it++)
    addAuthorization(*it);
}

void Authorizations::addAuthorization(std::string auth) {
  authStrings.insert(auth);
}

Authorizations::~Authorizations() {}
}  // namespace security
}  // namespace data
}  // namespace cclient
