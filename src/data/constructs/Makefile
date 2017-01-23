include Makefile.inc


CPPFLAGS=-g1 --std=c++11 -lthrift -I/usr/local/include/thrift/ -fPIC
LDFLAGS=-g1
# security/constructs.so server/constructs.so
LDLIBS= -shared -fPIC
RM=rm -f
PWD=`pwd`


DIRS	= server security rfile configuration compressor client
OBJS=$(subst .cpp,.o,$(wildcard *.cpp))
OBJLIBS	= libserver.so libsecurity.so librfile.so libconfiguration.so libcompressor.so libclient.so
LIBS	= server/server.so security/security.so rfile/rfile.so configuration/configuration.so compressor/compressors.so client/client.so

#SRCS=column.cpp Key.cpp KeyExtent.cpp KeyValue.cpp Mutation.cpp Range.cpp rkey.cpp scanstate.cpp value.cpp security/$(wildcard *.cpp)



all: constructs

	
constructs:	$(OBJS) $(OBJLIBS)
#	cd security; make clean ; make ; cd $(PWD)
#	cd server; make clean ; make ; cd $(PWD)
	$(ECHO) $(LD) -o constructs.so $(OBJS) $(LIBS) -shared -fPIC
	g++ -o constructs.so $(OBJS) $(LDFLAGS) -shared -fPIC
#	$(LD) -o $(EXE) $(OBJS) $(LIBS)
	$(RM) $(OBJS)
		


libserver.so : force_look
	$(ECHO) building libserver : $(MAKE) $(MFLAGS)
	cd server; $(MAKE) $(MFLAGS)
	
libsecurity.so : force_look
	$(ECHO) building libsecurity : $(MAKE) $(MFLAGS)
	cd security; $(MAKE) $(MFLAGS)
	
librfile.so : force_look
	$(ECHO) building librfile : $(MAKE) $(MFLAGS)
	cd rfile; $(MAKE) $(MFLAGS)
	
libconfiguration.so : force_look
	$(ECHO) building libconfiguration : $(MAKE) $(MFLAGS)
	cd configuration; $(MAKE) $(MFLAGS)

libcompressor.so : force_look
	$(ECHO) building libcompressor : $(MAKE) $(MFLAGS)
	cd compressor; $(MAKE) $(MFLAGS)

libclient.so : force_look
	$(ECHO) building libclient : $(MAKE) $(MFLAGS)
	cd client; $(MAKE) $(MFLAGS)

clean :
	$(ECHO) cleaning up in .
	-$(RM) -f $(EXE) $(OBJS) $(OBJLIBS)
	-for d in $(DIRS); do (cd $$d; $(MAKE) clean ); done

force_look :
	true
