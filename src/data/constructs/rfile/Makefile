CPPFLAGS=-g1 --std=c++11 -lthrift -I/usr/local/include/thrift/
LDFLAGS= ../../streaming/streaming.so ../compressor/compressor.so bcfile/bcfile.so meta/meta.so
LDLIBS=
RM=rm -f

SRCS=rfile_version.cpp RFile.cpp
OBJS=$(subst .cpp,.o,$(SRCS))


all: rfile

clean: 
	$(RM) $(OBJS)


rfile:	$(OBJS)
	cd ../../streaming/ ; make clean; make ; cd $(PWD)
	cd ../compressor/ ; make clean; make ; cd $(PWD)
	cd bcfile ; make clean; make ; cd $(PWD)
	cd meta ; make clean; make ; cd $(PWD)
	g++ -o rfile.so $(OBJS) -shared -fPIC
	$(RM) $(OBJS)

rfile_version.o: rfile_version.cpp
	g++ $(CPPFLAGS) -c rfile_version.cpp -fPIC

	
RFile.o: RFile.cpp
	g++ $(CPPFLAGS) -c RFile.cpp -fPIC
