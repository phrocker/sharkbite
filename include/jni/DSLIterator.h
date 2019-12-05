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

#ifndef INCLUDE_JNI_DSLITERATOR_H_
#define INCLUDE_JNI_DSLITERATOR_H_

#include <jni.h>
#include "data/constructs/Key.h"
#include "data/constructs/value.h"
#include "JNIUtil.h"


#ifdef __cplusplus
extern "C" {
#endif

JNIEXPORT void JNICALL Java_org_poma_accumulo_DSLIterator_setType(JNIEnv *, jobject, jstring);

JNIEXPORT void JNICALL Java_org_poma_accumulo_DSLIterator_setDSL(JNIEnv *, jobject, jstring);

JNIEXPORT void JNICALL Java_org_poma_accumulo_DSLIterator_init(JNIEnv *, jobject, jobject);

JNIEXPORT jobject JNICALL org_poma_accumulo_DSLIterator_getNextKey(JNIEnv*, jobject, jobject);

#ifdef __cplusplus
}
#endif

#endif /* INCLUDE_JNI_DSLITERATOR_H_ */
