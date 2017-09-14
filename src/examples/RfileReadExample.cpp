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
#include <fstream>
#include <iostream>
#include <sstream>

#include "../include/data/constructs/KeyValue.h"
#include "../include/data/constructs/security/Authorizations.h"
#include "../include/scanner/constructs/Results.h"
#include "../include/scanner/impl/Scanner.h"
#include "../include/data/constructs/client/zookeeperinstance.h"
#include "../include/interconnect/Master.h"
#include "../include/interconnect/tableOps/TableOperations.h"

#include "../include/data/constructs/rfile/RFile.h"
#include "../include/data/constructs/compressor/compressor.h"
#include "../include/data/constructs/compressor/zlibCompressor.h"

#include "../include/data/streaming/OutputStream.h"

#define BOOST_IOSTREAMS_NO_LIB 1



bool
keyCompare (std::shared_ptr<cclient::data::KeyValue>  a, std::shared_ptr<cclient::data::KeyValue>  b)
{
    return *(a->getKey ()) < *(b->getKey ());
}

void
writeRfile (std::string outputFile,bool bigEndian, uint16_t port)
{
    std::ofstream ofs (outputFile.c_str(), std::ofstream::out);

    cclient::data::streams::OutputStream *stream = new cclient::data::streams::OutputStream(&ofs,0);

    if (bigEndian)
        stream = new cclient::data::streams::EndianTranslationStream(stream);

    cclient::data::compression::Compressor *compressor = new cclient::data::compression::ZLibCompressor (256 * 1024);

    cclient::data::BlockCompressedFile bcFile (compressor);

// ByteOutputStream *outStream = new BigEndianByteStream (5 * 1024 * 1024,
//							 stream);
    cclient::data::RFile *newRFile = new cclient::data::RFile (stream, &bcFile);

    std::vector<std::shared_ptr<cclient::data::KeyValue> > keyValues;

    char rw[13], cf[4], cq[9], cv[9];
    int i = 0;

    std::string moto = "hello moto";
    std::string vis = "00000001";
    for (i = 1; i < 25; i++)
    {

        std::shared_ptr<cclient::data::Value> v = std::make_shared<cclient::data::Value> (moto);

        std::shared_ptr<cclient::data::Key> k = std::make_shared<cclient::data::Key> ();

        std::string rowSt = "2";

        memset (rw, 0x00, 13);
        sprintf (rw, "bat");

        k->setRow ((const char*) rw, 8);

        sprintf (cf, "%03d", i);

        k->setColFamily ((const char*) cf, 3);

        sprintf (cq, "%08d", i);
        sprintf (cv, "%08d", i);

        k->setColQualifier ((const char*) cq, 8);
        k->setColVisibility (vis.c_str (), vis.size ());

        k->setTimeStamp (1445105294261L);

        std::shared_ptr<cclient::data::KeyValue> kv = std::make_shared<cclient::data::KeyValue> ();

        kv->setKey (k);
        kv->setValue (v);

        keyValues.push_back (kv);
    }
    std::sort (keyValues.begin (), keyValues.end (), keyCompare);
    newRFile->addLocalityGroup ();
    for (std::vector<std::shared_ptr<cclient::data::KeyValue> >::iterator it = keyValues.begin ();
            it != keyValues.end (); ++it)
    {
        newRFile->append (*it);
    }

    stream->flush ();
    newRFile->close ();

    //outStream->flush ();


    //delete outStream;
    delete stream;

    delete newRFile;








}

std::ifstream::pos_type filesize(const char* filename)
{
    std::ifstream in(filename, std::ifstream::ate | std::ifstream::binary);
    return in.tellg();
}


void
readRfile (std::string outputFile, uint16_t port, bool bigEndian)
{

    std::ifstream ifs (outputFile.c_str(), std::ifstream::binary | std::ifstream::in);


    cclient::data::streams::InputStream *stream = new cclient::data::streams::InputStream(&ifs,0);

    if (bigEndian)
    {
        // stream = new EndianInputStream(stream);
    }


    std::fstream::pos_type size = filesize(outputFile.c_str());
    cclient::data::RFile *newRFile = new cclient::data::RFile (stream, size);
    std::vector<uint8_t*> cf;
    cclient::data::streams::StreamSeekable *seekable = new cclient::data::streams::StreamSeekable(new cclient::data::Range(),cf,false);


    newRFile->relocate(seekable);
    while (newRFile->hasNext())
    {
        std::cout << "has next " << (**newRFile).first <<  std::endl;

        newRFile->next();


        if (!newRFile->hasNext())
        {
            std::cout << "no next" << std::endl;
        }

    }





    std::cout << "we done" << std::endl;

    delete stream;

    delete newRFile;

}
int
main (int argc, char **argv)
{

    if (argc < 3)
    {
        std::cout << "Arguments required: ./RfileReadExample"
             << " <output file>" << std::endl;
        exit (1);
    }

    std::string outputFile = argv[1];

    std::string endian = argv[2];


    bool bigEndian = false;

    if (endian == "big" )
        bigEndian = true;
    if (!IsEmpty (&outputFile))
    {
        std::cout << "Writing test Rfile to " << outputFile << std::endl;
        writeRfile (outputFile, bigEndian, 0);
        if (bigEndian)
        {
            std::cout << "skipping read" << std::endl;
            //return 0;
        }
        std::cout << "Reading test rfile from " << outputFile << std::endl;
        readRfile(outputFile, 0 , bigEndian);
    }



    return 0;
}

